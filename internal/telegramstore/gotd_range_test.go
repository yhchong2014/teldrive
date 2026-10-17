package telegramstore

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestPlanTelegramReadsAlignsShortTail(t *testing.T) {
	plans := planTelegramReads(557056, 1234, defaultTelegramReadParallel)
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	plan := plans[0]
	if plan.offset != 557056 || plan.limit != telegramReadAlign || plan.skip != 0 || plan.length != 1234 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestGotdDownloadSessionRunsOneClientLifecycle(t *testing.T) {
	runner := &sessionCountingRunner{}
	storage := &GotdStorage{runner: runner}
	session, err := storage.OpenDownloadSession(context.Background(), 7)
	if err != nil {
		t.Fatalf("OpenDownloadSession() error = %v", err)
	}
	if got := runner.runs.Load(); got != 1 {
		t.Fatalf("runner calls = %d, want 1", got)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := runner.runs.Load(); got != 1 {
		t.Fatalf("runner calls after close = %d, want 1", got)
	}
}

func TestGotdDownloadSessionStopsWhenRequestIsCanceled(t *testing.T) {
	runner := &sessionCountingRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	session, err := (&GotdStorage{runner: runner}).OpenDownloadSession(ctx, 7)
	if err != nil {
		t.Fatalf("OpenDownloadSession() error = %v", err)
	}
	cancel()
	if err := session.Close(); err != nil {
		t.Fatalf("Close() after cancellation error = %v", err)
	}
	if got := runner.runs.Load(); got != 1 {
		t.Fatalf("runner calls = %d, want 1", got)
	}
}

type sessionCountingRunner struct{ runs atomic.Int32 }

func (r *sessionCountingRunner) Run(ctx context.Context, _ int64, operation Operation, fn func(context.Context, *tg.Client) error) error {
	r.runs.Add(1)
	if operation != OperationDownload {
		return ErrInvalidRequest
	}
	return fn(ctx, new(tg.Client))
}

func TestPlanTelegramReadsAlignsArbitraryRange(t *testing.T) {
	plans := planTelegramReads(101, telegramReadChunk+5000, defaultTelegramReadParallel)
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	first := plans[0]
	if first.offset != 0 || first.limit != telegramReadChunk || first.skip != 101 || first.length != telegramReadChunk-101 {
		t.Fatalf("first plan = %+v", first)
	}
	second := plans[1]
	if second.offset != telegramReadChunk || second.limit != 8192 || second.skip != 0 || second.length != 5101 {
		t.Fatalf("second plan = %+v", second)
	}
	for _, plan := range plans {
		if plan.offset%telegramReadAlign != 0 || plan.limit%telegramReadAlign != 0 || plan.limit > telegramReadChunk {
			t.Fatalf("unaligned plan = %+v", plan)
		}
	}
}

func TestPlanTelegramReadsStaysWithinTelegramBoundaries(t *testing.T) {
	plans := planTelegramReads(1474560, 13926400, defaultTelegramReadParallel)
	if len(plans) != defaultTelegramReadParallel {
		t.Fatalf("plans = %d, want %d", len(plans), defaultTelegramReadParallel)
	}
	for _, plan := range plans {
		if plan.offset%telegramReadAlign != 0 {
			t.Fatalf("unaligned offset: %+v", plan)
		}
		if plan.limit < telegramReadAlign || telegramReadChunk%plan.limit != 0 {
			t.Fatalf("invalid limit: %+v", plan)
		}
		end := plan.offset + int64(plan.limit) - 1
		if plan.offset/telegramReadChunk != end/telegramReadChunk {
			t.Fatalf("plan crosses 1 MiB boundary: %+v", plan)
		}
	}
}
func TestPlanTelegramReadRampsUpThenRealignsToFullChunks(t *testing.T) {
	var offset int64
	remaining := int64(3 * telegramReadChunk)
	var plans []telegramReadPlan
	for seq := int64(0); remaining > 0; seq++ {
		plan := planTelegramRead(offset, remaining, telegramReadLimit(seq))
		plans = append(plans, plan)
		offset += int64(plan.length)
		remaining -= int64(plan.length)
	}
	want := []telegramReadPlan{
		{offset: 0, limit: 128 * 1024, length: 128 * 1024},
		{offset: 128 * 1024, limit: 256 * 1024, length: 256 * 1024},
		{offset: 384 * 1024, limit: 512 * 1024, length: 512 * 1024},
		// The rest of the first MiB, cut by the 1 MiB request boundary.
		{offset: 896 * 1024, limit: 128 * 1024, length: 128 * 1024},
		{offset: telegramReadChunk, limit: telegramReadChunk, length: telegramReadChunk},
		{offset: 2 * telegramReadChunk, limit: telegramReadChunk, length: telegramReadChunk},
	}
	if len(plans) != len(want) {
		t.Fatalf("plans = %+v, want %+v", plans, want)
	}
	for i := range want {
		if plans[i] != want[i] {
			t.Fatalf("plan %d = %+v, want %+v", i, plans[i], want[i])
		}
	}
}

func TestPlanTelegramReadRampKeepsUnalignedStartWithinLimits(t *testing.T) {
	// A seek into the middle of a 4 KiB block still gets a small first request.
	plan := planTelegramRead(5*telegramReadChunk+4095, 10*telegramReadChunk, telegramReadLimit(0))
	if plan.offset != 5*telegramReadChunk || plan.skip != 4095 || plan.limit != 128*1024 || plan.length != 128*1024-4095 {
		t.Fatalf("plan = %+v", plan)
	}
	if telegramReadLimit(int64(len(telegramReadRamp))) != telegramReadChunk || telegramReadLimit(-1) != telegramReadChunk {
		t.Fatal("chunks after the ramp must use the full Telegram chunk size")
	}
}

func TestTelegramRangeReaderBoundsReadAheadBehindSlowChunk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	releaseFirst := make(chan struct{})
	invoker := &pipelinedDownloadInvoker{
		started: make(chan int64, 3),
		releases: map[int64]<-chan struct{}{
			0: releaseFirst,
		},
	}
	api := tg.NewClient(invoker)
	reader := newTelegramRangeReader(ctx, cancel, 4, 2)
	// Exactly three ramped chunks, so that one read is left after the head.
	// The first requests are capped by the read ramp, so the second chunk
	// starts right after the first ramped chunk.
	second := int64(telegramReadRamp[0])
	third := second + int64(telegramReadRamp[1])
	total := third + int64(telegramReadRamp[2])
	errCh := make(chan error, 1)
	go func() {
		errCh <- reader.fill(ctx, api, &tg.InputDocumentFileLocation{}, 0, total, nil)
	}()

	started := map[int64]bool{}
	for len(started) < 2 {
		select {
		case offset := <-invoker.started:
			started[offset] = true
		case <-time.After(time.Second):
			t.Fatal("initial Telegram reads did not start")
		}
	}
	if !started[0] || !started[second] {
		t.Fatalf("initial offsets = %#v", started)
	}
	select {
	case offset := <-invoker.started:
		t.Fatalf("read-ahead escaped ordered window while first chunk was stalled: offset %d", offset)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseFirst)
	select {
	case offset := <-invoker.started:
		if offset != third {
			t.Fatalf("next offset = %d, want %d", offset, third)
		}
	case <-time.After(time.Second):
		t.Fatal("next Telegram read did not start after stalled head completed")
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("fill() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fill() did not complete")
	}
}

func TestTelegramRangeReaderRetriesTimedOutChunk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	invoker := &timeoutThenSuccessDownloadInvoker{}
	api := tg.NewClient(invoker)
	reader := newTelegramRangeReader(ctx, cancel, 2, 1)
	reader.timeout = 10 * time.Millisecond
	reader.attempts = 2

	if err := reader.fill(ctx, api, &tg.InputDocumentFileLocation{}, 0, telegramReadAlign, nil); err != nil {
		t.Fatalf("fill() error = %v", err)
	}
	if got := invoker.calls.Load(); got != 2 {
		t.Fatalf("download calls = %d, want 2", got)
	}
}

func TestTelegramRangeReaderRetriesRPCTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	invoker := &rpcTimeoutThenSuccessDownloadInvoker{}
	api := tg.NewClient(invoker)
	reader := newTelegramRangeReader(ctx, cancel, 2, 1)
	reader.attempts = 2

	if err := reader.fill(ctx, api, &tg.InputDocumentFileLocation{}, 0, telegramReadAlign, nil); err != nil {
		t.Fatalf("fill() error = %v", err)
	}
	if got := invoker.calls.Load(); got != 2 {
		t.Fatalf("download calls = %d, want 2", got)
	}
}

func TestTelegramRangeReaderRefreshesExpiredFileReference(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	invoker := &expiringDownloadInvoker{}
	api := tg.NewClient(invoker)
	reader := newTelegramRangeReader(ctx, cancel, 2, 1)
	fresh := &tg.InputDocumentFileLocation{ID: 2}
	var refreshes atomic.Int32
	err := reader.fill(ctx, api, &tg.InputDocumentFileLocation{ID: 1}, 0, telegramReadAlign, func(context.Context) (*tg.InputDocumentFileLocation, error) {
		refreshes.Add(1)
		return fresh, nil
	})
	if err != nil {
		t.Fatalf("fill() error = %v", err)
	}
	if got := invoker.calls.Load(); got != 2 {
		t.Fatalf("download calls = %d, want 2", got)
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

type expiringDownloadInvoker struct {
	calls atomic.Int32
}

func (i *expiringDownloadInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	request := input.(*tg.UploadGetFileRequest)
	if i.calls.Add(1) == 1 {
		return tgerr.New(400, "FILE_REFERENCE_EXPIRED")
	}
	if request.Location.(*tg.InputDocumentFileLocation).ID != 2 {
		return tgerr.New(400, "FILE_REFERENCE_EXPIRED")
	}
	box := output.(*tg.UploadFileBox)
	box.File = &tg.UploadFile{Bytes: make([]byte, request.Limit)}
	return nil
}

type pipelinedDownloadInvoker struct {
	started  chan int64
	releases map[int64]<-chan struct{}
}

func (i *pipelinedDownloadInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	request := input.(*tg.UploadGetFileRequest)
	i.started <- request.Offset
	if release := i.releases[request.Offset]; release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	box := output.(*tg.UploadFileBox)
	box.File = &tg.UploadFile{Bytes: make([]byte, request.Limit)}
	return nil
}

type timeoutThenSuccessDownloadInvoker struct {
	calls atomic.Int32
}

func (i *timeoutThenSuccessDownloadInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	request := input.(*tg.UploadGetFileRequest)
	if i.calls.Add(1) == 1 {
		<-ctx.Done()
		return ctx.Err()
	}
	box := output.(*tg.UploadFileBox)
	box.File = &tg.UploadFile{Bytes: make([]byte, request.Limit)}
	return nil
}

type rpcTimeoutThenSuccessDownloadInvoker struct {
	calls atomic.Int32
}

func (i *rpcTimeoutThenSuccessDownloadInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	request := input.(*tg.UploadGetFileRequest)
	if i.calls.Add(1) == 1 {
		return tgerr.New(-503, "Timeout")
	}
	box := output.(*tg.UploadFileBox)
	box.File = &tg.UploadFile{Bytes: make([]byte, request.Limit)}
	return nil
}
