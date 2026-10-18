package telegramstore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

// accountLeaserStub leases accounts in rotation and runs a background client
// per RunPooled call until its context ends.
type accountLeaserStub struct {
	accounts []int64

	mu          sync.Mutex
	next        int
	active      map[int64]int
	leased      []int
	connections []int
	beforeReady func(context.Context)

	runs  atomic.Int32
	exits atomic.Int32
}

func newAccountLeaserStub(accounts ...int64) *accountLeaserStub {
	if len(accounts) == 0 {
		accounts = []int64{1}
	}
	return &accountLeaserStub{accounts: accounts, active: make(map[int64]int)}
}

func (r *accountLeaserStub) Run(ctx context.Context, _ int64, _ Operation, fn func(context.Context, *tg.Client) error) error {
	return fn(ctx, new(tg.Client))
}

func (r *accountLeaserStub) LeaseAccount(_ context.Context, _ int64, operation Operation, connections int) (AccountLease, error) {
	if operation != OperationDownload {
		return nil, ErrInvalidRequest
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.accounts[r.next%len(r.accounts)]
	r.next++
	r.active[account] += connections
	r.leased = append(r.leased, connections)
	return &accountLeaseStub{leaser: r, account: account, connections: connections}, nil
}

func (r *accountLeaserStub) activeLoad(account int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[account]
}

type accountLeaseStub struct {
	leaser      *accountLeaserStub
	account     int64
	connections int
	release     sync.Once
}

func (l *accountLeaseStub) AccountID() int64 { return l.account }

func (l *accountLeaseStub) RunPooled(ctx context.Context, connections int, fn func(context.Context, *tg.Client) error) error {
	l.leaser.mu.Lock()
	l.leaser.connections = append(l.leaser.connections, connections)
	l.leaser.mu.Unlock()
	l.leaser.runs.Add(1)
	defer l.leaser.exits.Add(1)
	if l.leaser.beforeReady != nil {
		l.leaser.beforeReady(ctx)
	}
	return fn(WithClientID(ctx, l.account), new(tg.Client))
}

func (l *accountLeaseStub) Release() {
	l.release.Do(func() {
		l.leaser.mu.Lock()
		l.leaser.active[l.account] -= l.connections
		l.leaser.mu.Unlock()
	})
}

func TestDownloadClientPoolReusesReleasedClientAfterRequestCancellation(t *testing.T) {
	runner := newAccountLeaserStub()
	pool := newTestDownloadClientPool(t, runner, DownloadClientPoolConfig{})
	storage := NewGotdStorage(runner, nil, WithDownloadClientPool(pool))

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	first, err := storage.OpenDownloadSession(requestCtx, 42)
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := storage.OpenDownloadSession(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.runs.Load(); got != 1 {
		t.Fatalf("runner starts = %d, want 1", got)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	closeDownloadClientPool(t, pool)
	if got := runner.exits.Load(); got != 1 {
		t.Fatalf("runner exits = %d, want 1", got)
	}
}

func TestDownloadClientPoolCountsOnlyOpenSessionsAsAccountLoad(t *testing.T) {
	runner := newAccountLeaserStub(1)
	pool := newTestDownloadClientPool(t, runner, DownloadClientPoolConfig{ReadParallel: 4})

	first, err := pool.OpenDownloadSession(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.OpenDownloadSession(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.activeLoad(1); got != 8 {
		t.Fatalf("account load with two open sessions = %d, want 8", got)
	}
	_ = first.Close()
	_ = first.Close()
	if got := runner.activeLoad(1); got != 4 {
		t.Fatalf("account load after closing one session twice = %d, want 4", got)
	}
	_ = second.Close()
	if got := runner.activeLoad(1); got != 0 {
		t.Fatalf("account load with only an idle warm client = %d, want 0", got)
	}
	if running := runner.runs.Load() - runner.exits.Load(); running != 1 {
		t.Fatalf("running warm clients = %d, want 1", running)
	}
	closeDownloadClientPool(t, pool)
}

func TestDownloadClientPoolSharesBackgroundClientAcrossConcurrentSessions(t *testing.T) {
	runner := newAccountLeaserStub()
	pool := newTestDownloadClientPool(t, runner, DownloadClientPoolConfig{})

	first, err := pool.OpenDownloadSession(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.OpenDownloadSession(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.runs.Load(); got != 1 {
		t.Fatalf("runner starts = %d, want 1 shared background client", got)
	}
	firstClient, err := first.(*gotdDownloadSession).client()
	if err != nil {
		t.Fatal(err)
	}
	secondClient, err := second.(*gotdDownloadSession).client()
	if err != nil {
		t.Fatal(err)
	}
	if firstClient != secondClient {
		t.Fatal("concurrent sessions do not share the background Telegram client")
	}
	_ = first.Close()
	_ = second.Close()
	closeDownloadClientPool(t, pool)
}

func TestDownloadClientPoolKeepsOneClientPerLeasedAccount(t *testing.T) {
	runner := newAccountLeaserStub(101, 102)
	pool := newTestDownloadClientPool(t, runner, DownloadClientPoolConfig{})

	var sessions []*gotdDownloadSession
	for range 3 {
		session, err := pool.OpenDownloadSession(context.Background(), 9)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, session.(*gotdDownloadSession))
	}
	if got := runner.runs.Load(); got != 2 {
		t.Fatalf("runner starts = %d, want 2 background clients", got)
	}
	for i, want := range []int64{101, 102, 101} {
		if got := sessions[i].clientID; got != want {
			t.Fatalf("session %d client ID = %d, want leased account %d", i, got, want)
		}
	}
	firstClient, _ := sessions[0].client()
	secondClient, _ := sessions[1].client()
	thirdClient, _ := sessions[2].client()
	if firstClient == secondClient {
		t.Fatal("different accounts share one background client")
	}
	if thirdClient != firstClient {
		t.Fatal("same account did not reuse its background client")
	}
	for _, session := range sessions {
		_ = session.Close()
	}
	closeDownloadClientPool(t, pool)
}

func TestDownloadClientPoolDistributesConnectionPoolAcrossAccounts(t *testing.T) {
	runner := newAccountLeaserStub(101, 102, 103)
	pool := newTestDownloadClientPool(t, runner, DownloadClientPoolConfig{ReadParallel: 16})

	session, err := pool.OpenDownloadSessionMulti(context.Background(), 9, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	connections := append([]int(nil), runner.connections...)
	runner.mu.Unlock()
	if len(connections) != 3 {
		t.Fatalf("pooled account count = %d, want 3", len(connections))
	}
	for _, got := range connections {
		if got != 6 {
			t.Errorf("per-account pool size = %d, want ceil(16/3)=6", got)
		}
	}
	_ = session.Close()
	closeDownloadClientPool(t, pool)
}

func TestDownloadClientPoolWarmUsesSmallPool(t *testing.T) {
	runner := newAccountLeaserStub(101)
	pool := newTestDownloadClientPool(t, runner, DownloadClientPoolConfig{ReadParallel: 16})
	if err := pool.Warm(context.Background(), 9, 101); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	connections := append([]int(nil), runner.connections...)
	runner.mu.Unlock()
	if len(connections) != 1 || connections[0] != 4 {
		t.Fatalf("warm pool sizes = %v, want [4]", connections)
	}
	closeDownloadClientPool(t, pool)
}

func TestDownloadClientPoolStartsColdAccountsConcurrently(t *testing.T) {
	runner := newAccountLeaserStub(101, 102, 103)
	started := atomic.Int32{}
	allStarted := make(chan struct{})
	runner.beforeReady = func(ctx context.Context) {
		if started.Add(1) == 3 {
			close(allStarted)
		}
		select {
		case <-allStarted:
		case <-ctx.Done():
		}
	}
	pool := newTestDownloadClientPool(t, runner, DownloadClientPoolConfig{ReadParallel: 6})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	session, err := pool.OpenDownloadSessionMulti(ctx, 9, 3, 0)
	if err != nil {
		t.Fatalf("OpenDownloadSessionMulti() = %v; all three clients should start before any is awaited", err)
	}
	if got := started.Load(); got != 3 {
		t.Fatalf("cold clients started = %d, want 3", got)
	}
	_ = session.Close()
	closeDownloadClientPool(t, pool)
}

func TestDownloadClientPoolReapsIdleClient(t *testing.T) {
	runner := newAccountLeaserStub(1)
	pool := newTestDownloadClientPool(t, runner, DownloadClientPoolConfig{})
	first, err := pool.OpenDownloadSession(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	pool.mu.Lock()
	pool.entries[downloadClientKey{userID: 10, accountID: 1}].lastUsed = time.Now().Add(-defaultDownloadClientIdleTimeout)
	pool.mu.Unlock()
	pool.reapIdle(time.Now())

	waitForExits(t, runner, 1)
	closeDownloadClientPool(t, pool)
}

func TestDownloadClientPoolRestartsOnlyStoppedAccountClient(t *testing.T) {
	runner := newAccountLeaserStub(1, 2)
	pool := newTestDownloadClientPool(t, runner, DownloadClientPoolConfig{})

	first, err := pool.OpenDownloadSession(context.Background(), 11)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.OpenDownloadSession(context.Background(), 11)
	if err != nil {
		t.Fatal(err)
	}
	secondClient, _ := second.(*gotdDownloadSession).client()
	_ = first.Close()
	_ = second.Close()

	pool.mu.Lock()
	pool.entries[downloadClientKey{userID: 11, accountID: 1}].lastUsed = time.Now().Add(-defaultDownloadClientIdleTimeout)
	pool.mu.Unlock()
	pool.reapIdle(time.Now())
	waitForExits(t, runner, 1)

	third, err := pool.OpenDownloadSession(context.Background(), 11)
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.runs.Load(); got != 3 {
		t.Fatalf("runner starts = %d, want one lazy restart for account 1", got)
	}
	_ = third.Close()

	fourth, err := pool.OpenDownloadSession(context.Background(), 11)
	if err != nil {
		t.Fatal(err)
	}
	fourthClient, _ := fourth.(*gotdDownloadSession).client()
	if fourthClient != secondClient {
		t.Fatal("healthy background client was restarted instead of reused")
	}
	if got := runner.runs.Load(); got != 3 {
		t.Fatalf("runner starts = %d, healthy client should not restart", got)
	}
	_ = fourth.Close()
	closeDownloadClientPool(t, pool)
}

func TestDownloadClientPoolUsesConfiguredConnections(t *testing.T) {
	runner := newAccountLeaserStub()
	pool := newTestDownloadClientPool(t, runner, DownloadClientPoolConfig{ReadParallel: 8})
	session, err := pool.OpenDownloadSession(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	leased, connections := runner.leased, runner.connections
	runner.mu.Unlock()
	if len(leased) != 1 || leased[0] != 8 || len(connections) != 1 || connections[0] != 8 {
		t.Fatalf("leased connections = %v, client connections = %v, want [8] and [8]", leased, connections)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	closeDownloadClientPool(t, pool)
}

func newTestDownloadClientPool(t *testing.T, leaser AccountLeaser, config DownloadClientPoolConfig) *DownloadClientPool {
	t.Helper()
	pool, err := NewDownloadClientPool(leaser, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func waitForExits(t *testing.T, runner *accountLeaserStub, want int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for runner.exits.Load() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := runner.exits.Load(); got != want {
		t.Fatalf("runner exits = %d, want %d", got, want)
	}
}

func closeDownloadClientPool(t *testing.T, pool *DownloadClientPool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := pool.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
