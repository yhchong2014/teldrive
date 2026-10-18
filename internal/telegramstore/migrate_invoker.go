package telegramstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ErrMigratingInvokerClosed is returned when a redirect arrives after the
// pooled client was closed.
var ErrMigratingInvokerClosed = errors.New("Telegram datacenter invoker is closed")

// datacenterRedirects lists the 303 errors that ask a client to repeat the same
// request against another datacenter.
var datacenterRedirects = []string{"FILE_MIGRATE", "STATS_MIGRATE"}

// migratingInvoker retries a request on the datacenter Telegram redirects it to.
//
// telegram.Client handles datacenter redirects in invokeDirect, but a pooled
// API is built on client.Pool, which is a bare multi-connection invoker that
// never passes through that logic. Media is stored in the datacenter that
// accepted the upload, so asking for it from another account - a second bot, or
// the user account - returns FILE_MIGRATE_X and would otherwise fail the
// request outright.
type documentRouteKey struct {
	id         int64
	accessHash int64
	thumbSize  string
}

const maxCachedDocumentRoutes = 256

type migratingInvoker struct {
	next   tg.Invoker
	openDC func(context.Context, int, int64) (telegram.CloseInvoker, error)
	size   int64

	mu         sync.Mutex
	dcs        map[int]telegram.CloseInvoker
	routes     map[documentRouteKey]int
	routeOrder []documentRouteKey
	closed     bool
}

func (m *migratingInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	key, hasRouteKey := getDocumentRouteKey(input)
	if hasRouteKey {
		if target := m.cachedDocumentDC(key); target > 0 {
			invoker, err := m.datacenter(ctx, target)
			if err != nil {
				return fmt.Errorf("open cached datacenter %d: %w", target, err)
			}
			err = invoker.Invoke(ctx, input, output)
			if err == nil {
				return nil
			}
			newTarget, ok := redirectTarget(err)
			if !ok || newTarget == target {
				return err
			}
			return m.retryRedirect(ctx, input, output, key, true, err)
		}
	}

	err := m.next.Invoke(ctx, input, output)
	if err == nil {
		return nil
	}
	return m.retryRedirect(ctx, input, output, key, hasRouteKey, err)
}

func getDocumentRouteKey(input bin.Encoder) (documentRouteKey, bool) {
	request, ok := input.(*tg.UploadGetFileRequest)
	if !ok {
		return documentRouteKey{}, false
	}
	location, ok := request.Location.(*tg.InputDocumentFileLocation)
	if !ok || location.ID == 0 || location.AccessHash == 0 {
		return documentRouteKey{}, false
	}
	return documentRouteKey{id: location.ID, accessHash: location.AccessHash, thumbSize: location.ThumbSize}, true
}

func (m *migratingInvoker) cachedDocumentDC(key documentRouteKey) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routes[key]
}

func (m *migratingInvoker) rememberDocumentDC(key documentRouteKey, dc int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if m.routes == nil {
		m.routes = make(map[documentRouteKey]int)
	}
	if _, exists := m.routes[key]; exists {
		m.routes[key] = dc
		return
	}
	if len(m.routes) >= maxCachedDocumentRoutes {
		oldest := m.routeOrder[0]
		m.routeOrder = m.routeOrder[1:]
		delete(m.routes, oldest)
	}
	m.routes[key] = dc
	m.routeOrder = append(m.routeOrder, key)
}

func (m *migratingInvoker) retryRedirect(ctx context.Context, input bin.Encoder, output bin.Decoder, key documentRouteKey, hasRouteKey bool, err error) error {
	// Telegram may chain redirects, so allow one retry per redirect.
	for attempt := 0; attempt < 2; attempt++ {
		target, ok := redirectTarget(err)
		if !ok {
			return err
		}
		// Media lives in the datacenter that accepted the upload, so this is
		// also the only place the file's actual location becomes visible.
		attrs := []any{"dc", target}
		if accountID, ok := ClientID(ctx); ok {
			attrs = append(attrs, "account_id", accountID)
		}
		slog.InfoContext(ctx, "telegram.file_datacenter", attrs...)
		invoker, openErr := m.datacenter(ctx, target)
		if openErr != nil {
			return errors.Join(err, fmt.Errorf("open datacenter %d: %w", target, openErr))
		}
		if hasRouteKey {
			m.rememberDocumentDC(key, target)
		}
		if err = invoker.Invoke(ctx, input, output); err == nil {
			return nil
		}
	}
	return err
}

// datacenter returns the invoker for dc, opening and caching a connection pool
// on first use. Concurrent chunk requests for the same file wait for that one
// pool instead of each paying the authorization transfer.
func (m *migratingInvoker) datacenter(ctx context.Context, dc int) (tg.Invoker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrMigratingInvokerClosed
	}
	if invoker, ok := m.dcs[dc]; ok {
		return invoker, nil
	}
	invoker, err := m.openDC(ctx, dc, m.size)
	if err != nil {
		return nil, err
	}
	if m.dcs == nil {
		m.dcs = make(map[int]telegram.CloseInvoker)
	}
	m.dcs[dc] = invoker
	return invoker, nil
}

func (m *migratingInvoker) Close() error {
	m.mu.Lock()
	m.closed = true
	dcs := m.dcs
	m.dcs = nil
	m.routes = nil
	m.routeOrder = nil
	m.mu.Unlock()

	var err error
	for _, invoker := range dcs {
		err = errors.Join(err, invoker.Close())
	}
	return err
}

// redirectTarget reports the datacenter a 303 redirect points at.
func redirectTarget(err error) (int, bool) {
	rpcErr, ok := tgerr.As(err)
	if !ok || rpcErr == nil || rpcErr.Argument <= 0 || !rpcErr.IsCode(303) {
		return 0, false
	}
	for _, kind := range datacenterRedirects {
		if rpcErr.IsType(kind) {
			return rpcErr.Argument, true
		}
	}
	return 0, false
}
