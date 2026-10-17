package telegramstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	// fileMigrateDialTimeout bounds opening a connection pool to another data
	// center, including the authorization transfer to it.
	fileMigrateDialTimeout = 30 * time.Second
	// maxFileMigrateRoutes bounds the remembered document data centers. The map
	// is cleared when it fills; routes are relearned with one extra round trip.
	maxFileMigrateRoutes = 4096
)

var errFileMigrateInvokerClosed = errors.New("Telegram connection pool is closed")

// dcDialer opens a connection pool to a Telegram data center with the
// authorization of the client that owns the primary pool.
type dcDialer func(ctx context.Context, dc int) (telegram.CloseInvoker, error)

// fileMigratingInvoker sends requests over the primary connection pool and
// retries requests rejected with FILE_MIGRATE_X on a pool to data center X.
//
// gotd follows FILE_MIGRATE only for calls made through the client's main
// connection. Calls made over client.Pool connections, which teldrive uses for
// parallel uploads and downloads, get the error back, so documents stored on a
// data center other than the account's home data center would fail to
// download. That is common: a file lives on the data center of whoever uploaded
// it, so files uploaded by a bot or imported from other channels often live
// elsewhere.
//
// After the first redirect for a document, later upload.getFile requests for it
// go straight to its data center.
type fileMigratingInvoker struct {
	primary telegram.CloseInvoker
	dial    dcDialer

	mu     sync.Mutex
	pools  map[int]*dcPool
	routes map[int64]int
	closed bool
}

type dcPool struct {
	ready   chan struct{}
	invoker telegram.CloseInvoker
	err     error
}

func newFileMigratingInvoker(primary telegram.CloseInvoker, dial dcDialer) *fileMigratingInvoker {
	return &fileMigratingInvoker{
		primary: primary, dial: dial,
		pools: make(map[int]*dcPool), routes: make(map[int64]int),
	}
}

func (m *fileMigratingInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	documentID, isDocumentRead := fileRequestDocumentID(input)
	if isDocumentRead {
		if dc, ok := m.route(documentID); ok {
			invoker, err := m.pool(ctx, dc)
			if err != nil {
				return err
			}
			err = invoker.Invoke(ctx, input, output)
			if !isFileMigrate(err) {
				return err
			}
			// A stale route: resolve the document again from the primary pool.
			m.forget(documentID)
		}
	}
	err := m.primary.Invoke(ctx, input, output)
	rpcErr, ok := tgerr.As(err)
	if !ok || rpcErr.Type != "FILE_MIGRATE" || rpcErr.Argument <= 0 {
		return err
	}
	dc := rpcErr.Argument
	invoker, poolErr := m.pool(ctx, dc)
	if poolErr != nil {
		return errors.Join(err, poolErr)
	}
	if isDocumentRead {
		m.remember(documentID, dc)
	}
	return invoker.Invoke(ctx, input, output)
}

func (m *fileMigratingInvoker) route(documentID int64) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dc, ok := m.routes[documentID]
	return dc, ok
}

func (m *fileMigratingInvoker) forget(documentID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.routes, documentID)
}

func isFileMigrate(err error) bool {
	rpcErr, ok := tgerr.As(err)
	return ok && rpcErr.Type == "FILE_MIGRATE"
}

func (m *fileMigratingInvoker) remember(documentID int64, dc int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.routes) >= maxFileMigrateRoutes {
		clear(m.routes)
	}
	m.routes[documentID] = dc
}

// pool returns the connection pool to dc, opening it on first use. Concurrent
// callers share one dial. The dial is detached from the caller's cancellation
// so that a client that disconnects does not fail the requests of others
// waiting for the same pool.
func (m *fileMigratingInvoker) pool(ctx context.Context, dc int) (tg.Invoker, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errFileMigrateInvokerClosed
	}
	existing := m.pools[dc]
	if existing == nil {
		created := &dcPool{ready: make(chan struct{})}
		m.pools[dc] = created
		m.mu.Unlock()

		dialCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fileMigrateDialTimeout)
		invoker, err := m.dial(dialCtx, dc)
		cancel()
		if err != nil {
			err = fmt.Errorf("connect to Telegram DC %d: %w", dc, err)
		}

		m.mu.Lock()
		switch {
		case err != nil:
			// Forget the failed dial so that a later request can try again.
			delete(m.pools, dc)
		case m.closed:
			_ = invoker.Close()
			invoker, err = nil, errFileMigrateInvokerClosed
		}
		created.invoker, created.err = invoker, err
		close(created.ready)
		m.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return invoker, nil
	}
	m.mu.Unlock()
	select {
	case <-existing.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if existing.err != nil {
		return nil, existing.err
	}
	return existing.invoker, nil
}

// Close closes the primary pool and every data center pool opened through it.
func (m *fileMigratingInvoker) Close() error {
	m.mu.Lock()
	m.closed = true
	pools := make([]*dcPool, 0, len(m.pools))
	for _, pool := range m.pools {
		pools = append(pools, pool)
	}
	clear(m.pools)
	m.mu.Unlock()

	err := m.primary.Close()
	for _, pool := range pools {
		select {
		case <-pool.ready:
			if pool.invoker != nil {
				err = errors.Join(err, pool.invoker.Close())
			}
		default:
			// Still dialing: the dialer sees closed and closes the pool itself.
		}
	}
	return err
}

// fileRequestDocumentID reports the document read by an upload.getFile request.
func fileRequestDocumentID(input bin.Encoder) (int64, bool) {
	request, ok := input.(*tg.UploadGetFileRequest)
	if !ok {
		return 0, false
	}
	location, ok := request.Location.(*tg.InputDocumentFileLocation)
	if !ok || location.ID == 0 {
		return 0, false
	}
	return location.ID, true
}

var _ telegram.CloseInvoker = (*fileMigratingInvoker)(nil)
