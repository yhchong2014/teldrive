package telegramstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type scriptedInvoker struct {
	calls   atomic.Int32
	closed  atomic.Bool
	respond func(input bin.Encoder) (bin.Encoder, error)
}

func (i *scriptedInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	i.calls.Add(1)
	response, err := i.respond(input)
	if err != nil {
		return err
	}
	var buffer bin.Buffer
	if err := response.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func (i *scriptedInvoker) Close() error {
	i.closed.Store(true)
	return nil
}

func fileMigrateError(dc string) error {
	return tgerr.New(303, "FILE_MIGRATE_"+dc)
}

func servesFile(payload []byte) func(bin.Encoder) (bin.Encoder, error) {
	return func(bin.Encoder) (bin.Encoder, error) {
		return &tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: payload}, nil
	}
}

func getFile(ctx context.Context, api *tg.Client, documentID int64) ([]byte, error) {
	response, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: documentID}, Limit: telegramReadAlign, Precise: true,
	})
	if err != nil {
		return nil, err
	}
	file, ok := response.(*tg.UploadFile)
	if !ok {
		return nil, errors.New("unexpected response")
	}
	return file.Bytes, nil
}

func TestFileMigratingInvokerRetriesOnTargetDCAndRemembersDocument(t *testing.T) {
	primary := &scriptedInvoker{respond: func(bin.Encoder) (bin.Encoder, error) { return nil, fileMigrateError("4") }}
	dc4 := &scriptedInvoker{respond: servesFile([]byte("dc4"))}
	var dials []int
	invoker := newFileMigratingInvoker(primary, func(_ context.Context, dc int) (telegram.CloseInvoker, error) {
		dials = append(dials, dc)
		return dc4, nil
	})
	api := tg.NewClient(invoker)
	ctx := context.Background()

	for range 3 {
		payload, err := getFile(ctx, api, 77)
		if err != nil {
			t.Fatalf("UploadGetFile() error = %v", err)
		}
		if string(payload) != "dc4" {
			t.Fatalf("payload = %q", payload)
		}
	}
	if len(dials) != 1 || dials[0] != 4 {
		t.Fatalf("dials = %v, want one dial to DC 4", dials)
	}
	// Only the first request learned the route; later ones go straight to DC 4.
	if got := primary.calls.Load(); got != 1 {
		t.Fatalf("primary calls = %d, want 1", got)
	}
	if got := dc4.calls.Load(); got != 3 {
		t.Fatalf("DC 4 calls = %d, want 3", got)
	}

	if err := invoker.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !primary.closed.Load() || !dc4.closed.Load() {
		t.Fatalf("Close() must close the primary and DC pools: primary=%v dc4=%v", primary.closed.Load(), dc4.closed.Load())
	}
	if _, err := getFile(ctx, api, 77); !errors.Is(err, errFileMigrateInvokerClosed) {
		t.Fatalf("UploadGetFile() after Close error = %v, want closed", err)
	}
}

func TestFileMigratingInvokerPassesThroughOtherErrors(t *testing.T) {
	primary := &scriptedInvoker{respond: func(bin.Encoder) (bin.Encoder, error) {
		return nil, tgerr.New(400, "FILE_REFERENCE_EXPIRED")
	}}
	invoker := newFileMigratingInvoker(primary, func(context.Context, int) (telegram.CloseInvoker, error) {
		t.Fatal("unexpected dial")
		return nil, nil
	})
	_, err := getFile(context.Background(), tg.NewClient(invoker), 1)
	if !tgerr.Is(err, "FILE_REFERENCE_EXPIRED") {
		t.Fatalf("error = %v, want FILE_REFERENCE_EXPIRED", err)
	}
}

func TestFileMigratingInvokerRedialsAfterFailedDial(t *testing.T) {
	primary := &scriptedInvoker{respond: func(bin.Encoder) (bin.Encoder, error) { return nil, fileMigrateError("2") }}
	dc2 := &scriptedInvoker{respond: servesFile([]byte("ok"))}
	dialErr := errors.New("dial failed")
	var dials atomic.Int32
	invoker := newFileMigratingInvoker(primary, func(context.Context, int) (telegram.CloseInvoker, error) {
		if dials.Add(1) == 1 {
			return nil, dialErr
		}
		return dc2, nil
	})
	api := tg.NewClient(invoker)

	_, err := getFile(context.Background(), api, 5)
	if !errors.Is(err, dialErr) || !tgerr.Is(err, "FILE_MIGRATE") {
		t.Fatalf("first error = %v, want dial error joined with FILE_MIGRATE", err)
	}
	if _, err := getFile(context.Background(), api, 5); err != nil {
		t.Fatalf("second UploadGetFile() error = %v", err)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dials = %d, want 2", got)
	}
}

func TestFileMigratingInvokerSharesOneDialAcrossConcurrentRequests(t *testing.T) {
	primary := &scriptedInvoker{respond: func(bin.Encoder) (bin.Encoder, error) { return nil, fileMigrateError("5") }}
	dc5 := &scriptedInvoker{respond: servesFile([]byte("dc5"))}
	release := make(chan struct{})
	var dials atomic.Int32
	invoker := newFileMigratingInvoker(primary, func(ctx context.Context, _ int) (telegram.CloseInvoker, error) {
		dials.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return dc5, nil
	})
	api := tg.NewClient(invoker)

	const requests = 8
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for id := range requests {
		wg.Go(func() {
			_, err := getFile(context.Background(), api, int64(100+id))
			errs <- err
		})
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("UploadGetFile() error = %v", err)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}
}

func TestFileMigratingInvokerRetriesNonDownloadRequestsWithoutRemembering(t *testing.T) {
	primary := &scriptedInvoker{respond: func(bin.Encoder) (bin.Encoder, error) { return nil, fileMigrateError("3") }}
	dc3 := &scriptedInvoker{respond: func(bin.Encoder) (bin.Encoder, error) { return &tg.BoolTrue{}, nil }}
	invoker := newFileMigratingInvoker(primary, func(context.Context, int) (telegram.CloseInvoker, error) { return dc3, nil })
	api := tg.NewClient(invoker)

	for range 2 {
		ok, err := api.UploadSaveBigFilePart(context.Background(), &tg.UploadSaveBigFilePartRequest{FileID: 1, FilePart: 0, FileTotalParts: 1, Bytes: []byte("x")})
		if err != nil || !ok {
			t.Fatalf("UploadSaveBigFilePart() = %v, %v", ok, err)
		}
	}
	if got := primary.calls.Load(); got != 2 {
		t.Fatalf("primary calls = %d, want 2 (only document reads are routed directly)", got)
	}
}

func TestFileMigratingInvokerReroutesStaleRoutes(t *testing.T) {
	var primaryDC atomic.Int32
	primaryDC.Store(4)
	primary := &scriptedInvoker{respond: func(bin.Encoder) (bin.Encoder, error) {
		return nil, fileMigrateError(fmt.Sprint(primaryDC.Load()))
	}}
	dc4 := &scriptedInvoker{respond: func(bin.Encoder) (bin.Encoder, error) { return nil, fileMigrateError("5") }}
	dc5 := &scriptedInvoker{respond: servesFile([]byte("dc5"))}
	pools := map[int]*scriptedInvoker{4: dc4, 5: dc5}
	invoker := newFileMigratingInvoker(primary, func(_ context.Context, dc int) (telegram.CloseInvoker, error) {
		return pools[dc], nil
	})
	invoker.remember(9, 4)
	primaryDC.Store(5)
	payload, err := getFile(context.Background(), tg.NewClient(invoker), 9)
	if err != nil || string(payload) != "dc5" {
		t.Fatalf("UploadGetFile() = %q, %v; want the document from DC 5", payload, err)
	}
	if dc, ok := invoker.route(9); !ok || dc != 5 {
		t.Fatalf("route = %d, %v; want DC 5", dc, ok)
	}
}
