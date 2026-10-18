package telegramstore

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// scriptedInvoker returns the queued errors and records how often it ran.
type scriptedInvoker struct {
	errs  []error
	calls int
}

func (s *scriptedInvoker) Invoke(context.Context, bin.Encoder, bin.Decoder) error {
	s.calls++
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

func (*scriptedInvoker) Close() error { return nil }

type closeTrackingInvoker struct {
	scriptedInvoker
	closed bool
}

func (c *closeTrackingInvoker) Close() error {
	c.closed = true
	return nil
}

func TestMigratingInvokerRetriesOnRedirectedDatacenter(t *testing.T) {
	t.Parallel()

	pooled := &scriptedInvoker{errs: []error{tgerr.New(303, "FILE_MIGRATE_4")}}
	target := &scriptedInvoker{}
	var opened []int
	invoker := &migratingInvoker{
		next: pooled,
		size: 8,
		openDC: func(_ context.Context, dc int, max int64) (telegram.CloseInvoker, error) {
			opened = append(opened, dc, int(max))
			return target, nil
		},
	}

	request := &tg.UploadGetFileRequest{}
	if err := invoker.Invoke(context.Background(), request, &tg.UploadFile{}); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if pooled.calls != 1 || target.calls != 1 {
		t.Fatalf("calls = pooled:%d target:%d, want 1 and 1", pooled.calls, target.calls)
	}
	if len(opened) != 2 || opened[0] != 4 || opened[1] != 8 {
		t.Fatalf("openDC() = %v, want [dc=4 max=8]", opened)
	}

	// A second redirect reuses the cached datacenter pool.
	pooled.errs = []error{tgerr.New(303, "FILE_MIGRATE_4")}
	if err := invoker.Invoke(context.Background(), request, &tg.UploadFile{}); err != nil {
		t.Fatalf("second Invoke() error = %v", err)
	}
	if len(opened) != 2 {
		t.Fatalf("openDC() called %d times, want the datacenter pool cached", len(opened)/2)
	}
}

func TestMigratingInvokerCachesDocumentDatacenterRoute(t *testing.T) {
	t.Parallel()

	pooled := &scriptedInvoker{errs: []error{tgerr.New(303, "FILE_MIGRATE_4")}}
	target := &scriptedInvoker{}
	opened := 0
	invoker := &migratingInvoker{
		next: pooled,
		openDC: func(_ context.Context, dc int, _ int64) (telegram.CloseInvoker, error) {
			if dc != 4 {
				t.Fatalf("openDC() datacenter = %d, want 4", dc)
			}
			opened++
			return target, nil
		},
	}
	request := &tg.UploadGetFileRequest{Location: &tg.InputDocumentFileLocation{ID: 10, AccessHash: 20}}

	for range 2 {
		if err := invoker.Invoke(context.Background(), request, &tg.UploadFile{}); err != nil {
			t.Fatalf("Invoke() error = %v", err)
		}
	}
	if pooled.calls != 1 || target.calls != 2 || opened != 1 {
		t.Fatalf("calls = pooled:%d target:%d openDC:%d, want 1, 2, 1", pooled.calls, target.calls, opened)
	}
}

func TestMigratingInvokerUpdatesCachedDocumentDatacenter(t *testing.T) {
	t.Parallel()

	pooled := &scriptedInvoker{errs: []error{tgerr.New(303, "FILE_MIGRATE_4")}}
	dc4 := &scriptedInvoker{}
	dc5 := &scriptedInvoker{}
	opened := make([]int, 0, 2)
	invoker := &migratingInvoker{
		next: pooled,
		openDC: func(_ context.Context, dc int, _ int64) (telegram.CloseInvoker, error) {
			opened = append(opened, dc)
			switch dc {
			case 4:
				return dc4, nil
			case 5:
				return dc5, nil
			default:
				return nil, fmt.Errorf("unexpected datacenter %d", dc)
			}
		},
	}
	request := &tg.UploadGetFileRequest{Location: &tg.InputDocumentFileLocation{ID: 10, AccessHash: 20}}
	if err := invoker.Invoke(context.Background(), request, &tg.UploadFile{}); err != nil {
		t.Fatalf("first Invoke() error = %v", err)
	}
	dc4.errs = []error{tgerr.New(303, "FILE_MIGRATE_5")}
	for range 2 {
		if err := invoker.Invoke(context.Background(), request, &tg.UploadFile{}); err != nil {
			t.Fatalf("Invoke() error = %v", err)
		}
	}
	if pooled.calls != 1 || dc4.calls != 2 || dc5.calls != 2 {
		t.Fatalf("calls = pooled:%d dc4:%d dc5:%d, want 1, 2, 2", pooled.calls, dc4.calls, dc5.calls)
	}
	if len(opened) != 2 || opened[0] != 4 || opened[1] != 5 {
		t.Fatalf("openDC() datacenters = %v, want [4 5]", opened)
	}
}

func TestMigratingInvokerReturnsOtherErrors(t *testing.T) {
	t.Parallel()

	flood := tgerr.New(420, "FLOOD_WAIT_5")
	invoker := &migratingInvoker{
		next: &scriptedInvoker{errs: []error{flood}},
		openDC: func(context.Context, int, int64) (telegram.CloseInvoker, error) {
			t.Fatal("openDC() called for a non-redirect error")
			return nil, nil
		},
	}
	if err := invoker.Invoke(context.Background(), &tg.UploadGetFileRequest{}, &tg.UploadFile{}); !errors.Is(err, flood) {
		t.Fatalf("Invoke() error = %v, want %v", err, flood)
	}
}

func TestMigratingInvokerReportsDatacenterFailure(t *testing.T) {
	t.Parallel()

	redirect := tgerr.New(303, "FILE_MIGRATE_2")
	openErr := errors.New("transfer failed")
	invoker := &migratingInvoker{
		next: &scriptedInvoker{errs: []error{redirect}},
		openDC: func(context.Context, int, int64) (telegram.CloseInvoker, error) {
			return nil, openErr
		},
	}
	err := invoker.Invoke(context.Background(), &tg.UploadGetFileRequest{}, &tg.UploadFile{})
	if !errors.Is(err, redirect) || !errors.Is(err, openErr) {
		t.Fatalf("Invoke() error = %v, want the redirect and the open failure", err)
	}
}

func TestMigratingInvokerClosesDatacenterPools(t *testing.T) {
	t.Parallel()

	target := &closeTrackingInvoker{}
	pooled := &scriptedInvoker{errs: []error{tgerr.New(303, "FILE_MIGRATE_3")}}
	invoker := &migratingInvoker{
		next: pooled,
		openDC: func(context.Context, int, int64) (telegram.CloseInvoker, error) {
			return target, nil
		},
	}
	if err := invoker.Invoke(context.Background(), &tg.UploadGetFileRequest{}, &tg.UploadFile{}); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if err := invoker.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !target.closed {
		t.Fatal("datacenter pool was not closed")
	}
	pooled.errs = []error{tgerr.New(303, "FILE_MIGRATE_3")}
	if err := invoker.Invoke(context.Background(), &tg.UploadGetFileRequest{}, &tg.UploadFile{}); !errors.Is(err, ErrMigratingInvokerClosed) {
		t.Fatalf("Invoke() after Close() error = %v, want %v", err, ErrMigratingInvokerClosed)
	}
}

func TestRedirectTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		dc   int
		ok   bool
	}{
		{name: "file migrate", err: tgerr.New(303, "FILE_MIGRATE_4"), dc: 4, ok: true},
		{name: "stats migrate", err: tgerr.New(303, "STATS_MIGRATE_2"), dc: 2, ok: true},
		{name: "wrapped", err: errors.Join(errors.New("chunk"), tgerr.New(303, "FILE_MIGRATE_5")), dc: 5, ok: true},
		{name: "flood wait", err: tgerr.New(420, "FLOOD_WAIT_5")},
		{name: "plain error", err: errors.New("boom")},
		{name: "nil", err: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dc, ok := redirectTarget(test.err)
			if ok != test.ok || dc != test.dc {
				t.Fatalf("redirectTarget(%v) = %d, %v; want %d, %v", test.err, dc, ok, test.dc, test.ok)
			}
		})
	}
}
