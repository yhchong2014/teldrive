package telegramstore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestSeekHedgeFastPrimaryDoesNotStartAlternate(t *testing.T) {
	var calls atomic.Int32
	got := raceSeekFetch(context.Background(), time.Second,
		func(context.Context) seekFetchResult {
			return seekFetchResult{payload: []byte("primary"), accountID: 1}
		},
		func(context.Context) seekFetchResult { calls.Add(1); return seekFetchResult{} })
	if got.err != nil || string(got.payload) != "primary" || calls.Load() != 0 {
		t.Fatalf("result = %+v, alternate calls = %d", got, calls.Load())
	}
}

func TestSeekHedgeCancelsSlowPrimaryAfterAlternateWins(t *testing.T) {
	cancelled := make(chan struct{})
	got := raceSeekFetch(context.Background(), 5*time.Millisecond,
		func(ctx context.Context) seekFetchResult {
			<-ctx.Done()
			close(cancelled)
			return seekFetchResult{err: ctx.Err()}
		},
		func(context.Context) seekFetchResult {
			return seekFetchResult{payload: []byte("alternate"), accountID: 2}
		})
	if got.err != nil || string(got.payload) != "alternate" || got.accountID != 2 {
		t.Fatalf("result = %+v", got)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("slow primary was not cancelled")
	}
}

func TestSeekHedgeFailureDoesNotMaskPrimary(t *testing.T) {
	release := make(chan struct{})
	failed := errors.New("alternate failed")
	got := raceSeekFetch(context.Background(), 5*time.Millisecond,
		func(context.Context) seekFetchResult { <-release; return seekFetchResult{payload: []byte("primary")} },
		func(context.Context) seekFetchResult { close(release); return seekFetchResult{err: failed} })
	if got.err != nil || string(got.payload) != "primary" {
		t.Fatalf("result = %+v", got)
	}
}

func TestSeekHedgeBothFailuresAndCancellation(t *testing.T) {
	primaryErr, alternateErr := errors.New("primary"), errors.New("alternate")
	got := raceSeekFetch(context.Background(), time.Second,
		func(context.Context) seekFetchResult { return seekFetchResult{err: primaryErr} },
		func(context.Context) seekFetchResult { return seekFetchResult{err: alternateErr} })
	if !errors.Is(got.err, primaryErr) || !errors.Is(got.err, alternateErr) {
		t.Fatalf("error = %v", got.err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got = raceSeekFetch(ctx, time.Second,
		func(ctx context.Context) seekFetchResult { <-ctx.Done(); return seekFetchResult{err: ctx.Err()} },
		func(context.Context) seekFetchResult { return seekFetchResult{} })
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("cancellation error = %v", got.err)
	}
}

type stalledSeekInvoker struct {
	cancelled chan struct{}
}

func (i *stalledSeekInvoker) Invoke(ctx context.Context, input bin.Encoder, _ bin.Decoder) error {
	if _, ok := input.(*tg.UploadGetFileRequest); !ok {
		return errors.New("unexpected request")
	}
	<-ctx.Done()
	close(i.cancelled)
	return ctx.Err()
}

func TestTelegramSeekUsesAlternateWhenFirstAccountStalls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	slow := &stalledSeekInvoker{cancelled: make(chan struct{})}
	fast := &fallbackDownloadInvoker{}
	clients := []rangeClient{{api: tg.NewClient(slow), clientID: 101}, {api: tg.NewClient(fast), clientID: 102}}
	reader := newTelegramRangeReader(ctx, cancel, 2, 2, nil)
	request := RangeRequest{ChannelID: 55, MessageID: 5, Offset: telegramReadAlign, Length: telegramReadAlign}
	if err := reader.fill(ctx, clients, request, &tg.InputDocumentFileLocation{ID: 1}, telegramReadAlign); err != nil {
		t.Fatalf("seek returned %v", err)
	}
	if fast.downloads.Load() != 1 {
		t.Fatalf("alternate downloads = %d", fast.downloads.Load())
	}
	select {
	case buffer := <-reader.buffers:
		if len(buffer.buf) != telegramReadAlign {
			t.Fatalf("buffer length = %d", len(buffer.buf))
		}
	default:
		t.Fatal("seek did not deliver data")
	}
	select {
	case <-slow.cancelled:
	case <-time.After(time.Second):
		t.Fatal("slow request was not cancelled")
	}
}

func TestTelegramSeekDoesNotHedgeWithParallelOne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, second := &fallbackDownloadInvoker{}, &fallbackDownloadInvoker{}
	clients := []rangeClient{{api: tg.NewClient(first), clientID: 1}, {api: tg.NewClient(second), clientID: 2}}
	reader := newTelegramRangeReader(ctx, cancel, 2, 1, nil)
	request := RangeRequest{ChannelID: 55, MessageID: 5, Offset: telegramReadAlign, Length: telegramReadAlign}
	if err := reader.fill(ctx, clients, request, &tg.InputDocumentFileLocation{ID: 1}, telegramReadAlign); err != nil {
		t.Fatal(err)
	}
	if first.downloads.Load() != 1 || second.downloads.Load() != 0 {
		t.Fatalf("downloads = %d, %d", first.downloads.Load(), second.downloads.Load())
	}
}
