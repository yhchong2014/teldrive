//go:build integration

package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/channels"
	"github.com/tgdrive/teldrive/v2/internal/jobs"
	"github.com/tgdrive/teldrive/v2/internal/localtelegram"
	"github.com/tgdrive/teldrive/v2/internal/telegramimport"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
	testpostgres "github.com/tgdrive/teldrive/v2/internal/testutil/postgres"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

func TestTelegramImportWorkerImportsIntoDestinationPath(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	server, err := localtelegram.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := localtelegram.NewRunner(server)
	if err != nil {
		t.Fatal(err)
	}
	storage := telegramstore.NewGotdStorage(runner, nil)
	source, err := storage.CreateChannel(ctx, 1001, "collection")
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("teldrive"), 9000)
	stored, err := storage.Upload(ctx, telegramstore.UploadRequest{
		UserID: 1001, ChannelID: source.ID, Name: "episode-01.mkv", Reader: bytes.NewReader(payload), Size: int64(len(payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	catalogService := catalog.NewService(db.Pool, nil)
	uploadService := uploads.NewService(db.Pool, 7*24*time.Hour)
	// No storage channel is registered: the copy goes to an automatically
	// created one.
	channelService := channels.NewService(db.Pool, channels.TelegramCreator{Storage: storage}, channels.Config{PartLimit: 1000, AutoCreate: true, NamePrefix: "teldrive"})
	importer, err := telegramimport.NewService(db.Pool, uploadService, channelService, storage)
	if err != nil {
		t.Fatal(err)
	}
	worker := jobs.NewTelegramImportWorker(catalogService, importer)

	job := &river.Job[jobs.TelegramImportArgs]{Args: jobs.TelegramImportArgs{
		UserID: 1001, Destination: "/Telegram/Collection",
		Sources: []telegramimport.Source{{ChannelID: source.ID, Messages: []telegramimport.MessageRange{{From: stored.MessageID, To: stored.MessageID}}}},
	}}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	folder, err := catalogService.ResolveFolderPath(ctx, 1001, nil, "/Telegram/Collection")
	if err != nil || folder == nil {
		t.Fatalf("destination folder = %v, %v", folder, err)
	}
	file, err := catalogService.ResolveChild(ctx, 1001, folder, "episode-01.mkv")
	if err != nil {
		t.Fatalf("imported file: %v", err)
	}
	if file.Size.Int64 != int64(len(payload)) {
		t.Fatalf("imported size = %d, want %d", file.Size.Int64, len(payload))
	}

	invalid := &river.Job[jobs.TelegramImportArgs]{Args: jobs.TelegramImportArgs{
		UserID: 1001, Destination: "/Telegram", Mode: "move",
		Sources: []telegramimport.Source{{ChannelID: source.ID, Scan: true}},
	}}
	var cancelled *river.JobCancelError
	if err := worker.Work(ctx, invalid); !errors.As(err, &cancelled) {
		t.Fatalf("Work(invalid mode) error = %v, want a cancelled job", err)
	}
}
