package telegramstore

import (
	"context"
	"errors"
	"fmt"
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
type migratingInvoker struct {
	next   tg.Invoker
	openDC func(context.Context, int, int64) (telegram.CloseInvoker, error)
	size   int64

	mu     sync.Mutex
	dcs    map[int]telegram.CloseInvoker
	closed bool
}

func (m *migratingInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	err := m.next.Invoke(ctx, input, output)
	// Telegram may chain redirects, so allow one retry per redirect.
	for attempt := 0; attempt < 2; attempt++ {
		target, ok := redirectTarget(err)
		if !ok {
			return err
		}
		invoker, openErr := m.datacenter(ctx, target)
		if openErr != nil {
			return errors.Join(err, fmt.Errorf("open datacenter %d: %w", target, openErr))
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
