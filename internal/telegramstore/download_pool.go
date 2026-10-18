package telegramstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/tgdrive/teldrive/v2/internal/cache"
)

const defaultDownloadClientIdleTimeout = 30 * time.Minute

var ErrDownloadClientPoolClosed = errors.New("Telegram download client pool is closed")

// AccountLease reserves active load on the Telegram account chosen for one unit
// of work until Release is called.
type AccountLease interface {
	// AccountID identifies the Telegram account (user or bot) behind the lease.
	AccountID() int64
	// RunPooled runs fn on a client bound to the leased account. The client may
	// keep running after Release; only leased work counts as account load, not
	// the idle client.
	RunPooled(ctx context.Context, connections int, fn func(context.Context, *tg.Client) error) error
	// Release returns the reserved load. It is safe to call more than once.
	Release()
}

// AccountLeaser chooses the Telegram account that serves the next unit of work.
type AccountLeaser interface {
	LeaseAccount(ctx context.Context, userID int64, operation Operation, connections int) (AccountLease, error)
}

// MultiAccountLeaser selects and reserves several accounts in one operation.
// Implementations can batch account lookups and allocate the total connection
// budget across the selected accounts instead of charging every account for
// the full stream parallelism.
type MultiAccountLeaser interface {
	LeaseAccounts(ctx context.Context, userID int64, operation Operation, accountLimit, totalConnections int, preferredAccountID int64) ([]AccountLease, error)
}

// DownloadAccountLister reports the accounts that can be used to size a warm
// client's pool before a stream is opened.
type DownloadAccountLister interface {
	DownloadAccounts(ctx context.Context, userID int64) ([]int64, error)
}

// PreferredAccountLeaser optionally pins a download to a previously chosen
// account so repeated HTTP requests for the same media file keep using the warm
// client that served the first one.
type PreferredAccountLeaser interface {
	LeasePreferredAccount(ctx context.Context, userID int64, operation Operation, connections int, preferred int64) (AccountLease, error)
}

// MultiAccountDownloadSession is implemented by the production client pool. A
// single stream may fan its chunk requests across several Telegram accounts.
type MultiAccountDownloadSession interface {
	OpenDownloadSessionMulti(ctx context.Context, userID int64, accounts int, preferredAccountID int64) (DownloadSession, error)
}

type DownloadClientPoolConfig struct {
	ReadBuffers  int
	ReadParallel int
	IdleTimeout  time.Duration
}

// DownloadClientPool keeps one authenticated download client warm per user and
// Telegram account. Each download session leases an account from the
// AccountLeaser, so account selection happens per session, and reuses the warm
// client for that account or starts one.
type DownloadClientPool struct {
	leaser      AccountLeaser
	config      DownloadClientPoolConfig
	globalCache cache.Cacher
	ctx         context.Context
	cancel      context.CancelFunc

	mu      sync.Mutex
	entries map[downloadClientKey]*downloadClientEntry
	closed  bool
	done    chan struct{}
}

type downloadClientKey struct {
	userID    int64
	accountID int64
}

type downloadClientEntry struct {
	key      downloadClientKey
	clientID int64
	ctx      context.Context
	cancel   context.CancelFunc
	ready    chan struct{}
	done     chan struct{}
	api      *tg.Client
	err      error
	refs     int
	lastUsed time.Time
}

func NewDownloadClientPool(leaser AccountLeaser, config DownloadClientPoolConfig, c cache.Cacher) (*DownloadClientPool, error) {
	if leaser == nil {
		return nil, ErrInvalidRequest
	}
	if config.ReadBuffers <= 0 {
		config.ReadBuffers = defaultTelegramReadBuffers
	}
	if config.ReadParallel <= 0 {
		config.ReadParallel = defaultTelegramReadParallel
	}
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = defaultDownloadClientIdleTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	pool := &DownloadClientPool{
		leaser: leaser, config: config, globalCache: c, ctx: ctx, cancel: cancel,
		entries: make(map[downloadClientKey]*downloadClientEntry), done: make(chan struct{}),
	}
	go pool.reap()
	return pool, nil
}

// OpenDownloadSession leases an account and returns a session on its warm
// client. When the leased account turns out to be revoked before the client is
// ready, the leaser has already dropped it, so another account is leased.
func (p *DownloadClientPool) OpenDownloadSession(ctx context.Context, userID int64) (DownloadSession, error) {
	return p.OpenDownloadSessionMulti(ctx, userID, 1, 0)
}

// OpenDownloadSessionMulti leases up to accounts distinct Telegram accounts and
// returns one session that rotates chunk requests across all of them. A
// preferred account, typically carried by a signed stream token, is used first
// so repeated requests for the same file reuse the same warm client.
func (p *DownloadClientPool) OpenDownloadSessionMulti(ctx context.Context, userID int64, accounts int, preferredAccountID int64) (DownloadSession, error) {
	if p == nil || userID <= 0 {
		return nil, ErrInvalidRequest
	}
	if accounts < 1 {
		accounts = 1
	}
	for {
		session, err := p.openDownloadSessionMulti(ctx, userID, accounts, preferredAccountID, p.config.ReadParallel)
		if !errors.Is(err, ErrAccountRevoked) {
			return session, err
		}
	}
}

func (p *DownloadClientPool) openDownloadSessionMulti(ctx context.Context, userID int64, accounts int, preferredAccountID int64, totalConnections int) (DownloadSession, error) {
	leases := make([]AccountLease, 0, accounts)
	var firstErr error
	if totalConnections < 1 {
		totalConnections = 1
	}
	if multi, ok := p.leaser.(MultiAccountLeaser); ok {
		var err error
		leases, err = multi.LeaseAccounts(ctx, userID, OperationDownload, accounts, totalConnections, preferredAccountID)
		if err != nil {
			for _, lease := range leases {
				lease.Release()
			}
			return nil, err
		}
	} else {
		seen := make(map[int64]struct{}, accounts)
		if preferredAccountID > 0 {
			lease, err := p.leaseAccount(ctx, userID, preferredAccountID, totalConnections)
			if err == nil {
				leases = append(leases, lease)
				seen[lease.AccountID()] = struct{}{}
			}
		}
		for len(leases) < accounts {
			lease, err := p.leaser.LeaseAccount(ctx, userID, OperationDownload, totalConnections)
			if err != nil {
				firstErr = err
				break
			}
			if _, duplicate := seen[lease.AccountID()]; duplicate {
				lease.Release()
				break
			}
			seen[lease.AccountID()] = struct{}{}
			leases = append(leases, lease)
		}
	}
	if len(leases) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, ErrClientUnavailable
	}

	// ReadParallel is the total number of requests this stream can have in
	// flight. Distribute that pool capacity across leased accounts instead of
	// creating ReadParallel connections for every bot (12 bots × 16 would be
	// 192 connections for a single stream).
	poolConnections := (totalConnections + len(leases) - 1) / len(leases)
	if poolConnections < 1 {
		poolConnections = 1
	}

	candidates := make([]*pooledDownloadClient, 0, len(leases))
	for _, lease := range leases {
		entry, created, err := p.lease(downloadClientKey{userID: userID, accountID: lease.AccountID()})
		if err != nil {
			lease.Release()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		candidate := &pooledDownloadClient{entry: entry, lease: lease}
		candidates = append(candidates, candidate)
		if created {
			// Start every cold account immediately, but do not make VLC wait for
			// the slowest account before the first ready client can serve bytes.
			p.start(entry, lease, poolConnections)
		}
	}
	if len(candidates) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, ErrClientUnavailable
	}

	clients := newPooledDownloadClients(ctx, p, candidates, firstErr)
	ready, err := clients.wait(ctx, nil)
	if err != nil {
		_ = clients.Close()
		return nil, err
	}
	initial := ready[0]
	return &gotdDownloadSession{
		clients: []sessionClient{{api: initial.api, clientID: initial.clientID}},
		clientID:             initial.clientID,
		clientSource:         clients.wait,
		closeFn:              clients.Close,
		downloadReadBuffers:  p.config.ReadBuffers,
		downloadReadParallel: p.config.ReadParallel,
		globalCache:          p.globalCache,
		stats:                newStreamStats(),
	}, nil
}

type pooledDownloadClient struct {
	entry    *downloadClientEntry
	lease    AccountLease
	api      *tg.Client
	clientID int64
	ready    bool
	release  sync.Once
}

func (c *pooledDownloadClient) releaseLease(pool *DownloadClientPool) {
	if c == nil {
		return
	}
	c.release.Do(func() {
		pool.release(c.entry)
		c.lease.Release()
	})
}

type pooledDownloadClients struct {
	pool       *DownloadClientPool
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	candidates []*pooledDownloadClient
	remaining  int
	firstErr   error
	changed    chan struct{}
	closeOnce  sync.Once
}

func newPooledDownloadClients(ctx context.Context, pool *DownloadClientPool, candidates []*pooledDownloadClient, firstErr error) *pooledDownloadClients {
	setupCtx, cancel := context.WithCancel(ctx)
	clients := &pooledDownloadClients{
		pool: pool, ctx: setupCtx, cancel: cancel,
		candidates: candidates, remaining: len(candidates), firstErr: firstErr,
		changed: make(chan struct{}),
	}
	for _, candidate := range candidates {
		go clients.watch(candidate)
	}
	return clients
}

func (s *pooledDownloadClients) watch(candidate *pooledDownloadClient) {
	select {
	case <-candidate.entry.ready:
		s.pool.mu.Lock()
		api, clientID, runErr := candidate.entry.api, candidate.entry.clientID, candidate.entry.err
		s.pool.mu.Unlock()

		s.mu.Lock()
		candidate.api, candidate.clientID, candidate.ready = api, clientID, true
		s.remaining--
		if api == nil && runErr != nil && s.firstErr == nil {
			s.firstErr = runErr
		}
		s.mu.Unlock()
		if api == nil {
			candidate.releaseLease(s.pool)
		}
		s.signal()
	case <-s.ctx.Done():
	}
}

func (s *pooledDownloadClients) signal() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func (s *pooledDownloadClients) wait(ctx context.Context, excluded map[int64]struct{}) ([]rangeClient, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		changed := s.changed
		s.mu.Unlock()

		clients := s.available(excluded)
		if len(clients) > 0 {
			return clients, nil
		}

		s.mu.Lock()
		if s.changed != changed {
			s.mu.Unlock()
			continue
		}
		remaining, firstErr := s.remaining, s.firstErr
		s.mu.Unlock()
		if remaining == 0 {
			if firstErr != nil {
				return nil, firstErr
			}
			return nil, ErrClientUnavailable
		}

		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.ctx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrClientUnavailable
		case <-s.pool.ctx.Done():
			return nil, ErrDownloadClientPoolClosed
		}
	}
}

func (s *pooledDownloadClients) available(excluded map[int64]struct{}) []rangeClient {
	type candidateClient struct {
		candidate *pooledDownloadClient
		api       *tg.Client
		clientID  int64
	}
	s.mu.Lock()
	ready := make([]candidateClient, 0, len(s.candidates))
	for _, candidate := range s.candidates {
		if candidate.ready && candidate.api != nil {
			ready = append(ready, candidateClient{candidate: candidate, api: candidate.api, clientID: candidate.clientID})
		}
	}
	s.mu.Unlock()

	clients := make([]rangeClient, 0, len(ready))
	for _, candidate := range ready {
		if _, skip := excluded[candidate.clientID]; skip {
			continue
		}
		if _, err := s.pool.client(candidate.candidate.entry, candidate.api); err != nil {
			continue
		}
		clients = append(clients, rangeClient{api: candidate.api, clientID: candidate.clientID})
	}
	return clients
}

func (s *pooledDownloadClients) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		candidates := append([]*pooledDownloadClient(nil), s.candidates...)
		s.mu.Unlock()
		for _, candidate := range candidates {
			candidate.releaseLease(s.pool)
		}
	})
	return nil
}

func (p *DownloadClientPool) leaseAccount(ctx context.Context, userID, preferredAccountID int64, connections int) (AccountLease, error) {
	if preferred, ok := p.leaser.(PreferredAccountLeaser); ok {
		return preferred.LeasePreferredAccount(ctx, userID, OperationDownload, connections, preferredAccountID)
	}
	return p.leaser.LeaseAccount(ctx, userID, OperationDownload, connections)
}

// Warm connects the download client for an account without starting a stream.
// The pool keeps it alive afterwards so the player's first request does not pay
// the Telegram connect and authentication cost.
func (p *DownloadClientPool) Warm(ctx context.Context, userID, accountID int64) error {
	session, err := p.openWarmSession(ctx, userID, accountID)
	if err != nil {
		return err
	}
	return session.Close()
}

func (p *DownloadClientPool) openWarmSession(ctx context.Context, userID, accountID int64) (DownloadSession, error) {
	if p == nil || userID <= 0 {
		return nil, ErrInvalidRequest
	}
	// A warm-up only needs the preferred account. Size its pool for its likely
	// share of the stream, with a small floor so the pinned client is not
	// under-provisioned when the stream fans out across bots.
	connections := min(p.config.ReadParallel, 4)
	if lister, ok := p.leaser.(DownloadAccountLister); ok {
		if accounts, err := lister.DownloadAccounts(ctx, userID); err == nil && len(accounts) > 0 {
			perAccount := (p.config.ReadParallel + len(accounts) - 1) / len(accounts)
			connections = max(connections, perAccount)
		}
	}
	if connections < 1 {
		connections = 1
	}
	return p.openDownloadSessionMulti(ctx, userID, 1, accountID, connections)
}

// lease returns the live warm client for key, or registers a new one that the
// caller must start.
func (p *DownloadClientPool) lease(key downloadClientKey) (*downloadClientEntry, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false, ErrDownloadClientPoolClosed
	}
	if entry := p.entries[key]; entry != nil && entry.err == nil && entry.ctx.Err() == nil {
		entry.refs++
		return entry, false, nil
	}
	entryCtx, cancel := context.WithCancel(p.ctx)
	entry := &downloadClientEntry{
		key: key, ctx: entryCtx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}), refs: 1, lastUsed: time.Now(),
	}
	p.entries[key] = entry
	return entry, true, nil
}

// start runs the warm client through the lease that created it. The client
// outlives that lease; later sessions take their own leases on the account.
func (p *DownloadClientPool) start(entry *downloadClientEntry, lease AccountLease, connections int) {
	go func() {
		ready := sync.Once{}
		err := lease.RunPooled(entry.ctx, connections, func(runCtx context.Context, api *tg.Client) error {
			p.mu.Lock()
			entry.api = api
			entry.clientID, _ = ClientID(runCtx)
			p.mu.Unlock()
			ready.Do(func() { close(entry.ready) })
			<-runCtx.Done()
			return runCtx.Err()
		})
		p.mu.Lock()
		entry.api = nil
		entry.err = fmt.Errorf("run pooled Telegram download client: %w", err)
		if p.entries[entry.key] == entry {
			delete(p.entries, entry.key)
		}
		p.mu.Unlock()
		ready.Do(func() { close(entry.ready) })
		close(entry.done)
	}()
}

func (p *DownloadClientPool) client(entry *downloadClientEntry, expected *tg.Client) (*tg.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || entry.refs <= 0 || entry.api == nil || entry.api != expected || entry.err != nil {
		return nil, ErrClientUnavailable
	}
	return entry.api, nil
}

func (p *DownloadClientPool) release(entry *downloadClientEntry) {
	p.mu.Lock()
	if entry.refs > 0 {
		entry.refs--
		if entry.refs == 0 {
			entry.lastUsed = time.Now()
		}
	}
	p.mu.Unlock()
}

func (p *DownloadClientPool) reap() {
	interval := p.config.IdleTimeout / 2
	ticker := time.NewTicker(interval)
	defer func() {
		ticker.Stop()
		close(p.done)
	}()
	for {
		select {
		case now := <-ticker.C:
			p.reapIdle(now)
		case <-p.ctx.Done():
			return
		}
	}
}

func (p *DownloadClientPool) reapIdle(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.entries {
		if entry.refs == 0 && now.Sub(entry.lastUsed) >= p.config.IdleTimeout {
			entry.cancel()
		}
	}
}

func (p *DownloadClientPool) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		p.cancel()
	}
	entries := make([]*downloadClientEntry, 0, len(p.entries))
	for _, entry := range p.entries {
		entries = append(entries, entry)
	}
	p.mu.Unlock()

	for _, entry := range entries {
		select {
		case <-entry.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
