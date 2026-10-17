//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/channels"
	"github.com/tgdrive/teldrive/v2/internal/jobs"
	"github.com/tgdrive/teldrive/v2/internal/localtelegram"
	"github.com/tgdrive/teldrive/v2/internal/principal"
	"github.com/tgdrive/teldrive/v2/internal/telegramimport"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
	testpostgres "github.com/tgdrive/teldrive/v2/internal/testutil/postgres"
	"github.com/tgdrive/teldrive/v2/internal/transfer"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

func TestCreateTelegramImportQueuesOwnedJob(t *testing.T) {
	db := testpostgres.New(t)
	ctx := principal.WithIdentity(context.Background(), principal.Identity{UserID: 1001})
	if _, err := db.Pool.Exec(context.Background(), "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	server, err := localtelegram.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := localtelegram.NewRunner(server)
	if err != nil {
		t.Fatal(err)
	}
	storage := telegramstore.NewGotdStorage(runner, nil)
	uploadService := uploads.NewService(db.Pool, 7*24*time.Hour)
	catalogService := catalog.NewService(db.Pool, nil)
	channelService := channels.NewService(db.Pool, channels.TelegramCreator{Storage: storage}, channels.Config{PartLimit: 1000, NamePrefix: "teldrive"})

	// Without the import service the endpoint is unavailable.
	plain, err := jobs.NewRuntime(db.Pool, storage)
	if err != nil {
		t.Fatal(err)
	}
	request := &gen.TelegramImportRequest{Destination: "/Imported", Links: []string{"https://t.me/c/1234567890/45-47"}}
	_, err = (&Handler{Jobs: plain}).CreateTelegramImport(ctx, request)
	var unavailable *Problem
	if !errors.As(err, &unavailable) || unavailable.Status != http.StatusServiceUnavailable {
		t.Fatalf("CreateTelegramImport() without imports error = %v, want 503", err)
	}

	importer, err := telegramimport.NewService(db.Pool, uploadService, channelService, storage)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := transfer.NewPipeline(uploadService, channelService, storage, nil, transfer.Config{})
	runtime, err := jobs.NewRuntimeWithServices(db.Pool, storage, "teldrive", nil, nil, 7*24*time.Hour, jobs.UploaderServices{
		Catalog: catalogService, Uploads: uploadService, Pipeline: pipeline, TelegramImports: importer,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{Jobs: runtime}

	for _, invalid := range []*gen.TelegramImportRequest{
		{Destination: "relative/path", Links: request.Links},
		{Destination: "/Imported"},
		{Destination: "/Imported", Links: []string{"https://example.com/c/1/2"}},
		{Destination: "/Imported", Channel: gen.NewOptString("123")},
	} {
		_, err := handler.CreateTelegramImport(ctx, invalid)
		var problem *Problem
		if !errors.As(err, &problem) || problem.Status != http.StatusUnprocessableEntity || strings.Contains(problem.Message, "invalid Telegram import source") {
			t.Fatalf("CreateTelegramImport(%+v) error = %v, want a readable 422", invalid, err)
		}
	}

	request.Channel = gen.NewOptString("-1001234567890")
	request.Scan = gen.NewOptBool(true)
	request.Mode = gen.NewOptTelegramImportRequestMode(gen.TelegramImportRequestModeCopy)
	response, err := handler.CreateTelegramImport(ctx, request)
	if err != nil {
		t.Fatalf("CreateTelegramImport() error = %v", err)
	}
	job, ok := response.(*gen.Job)
	if !ok || job.Type != jobs.TelegramImportKind {
		t.Fatalf("response = %#v", response)
	}
	var args jobs.TelegramImportArgs
	fields := make(map[string]json.RawMessage, len(job.Args))
	for key, value := range job.Args {
		fields[key] = json.RawMessage(value)
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	want := telegramimport.Source{ChannelID: 1234567890, Scan: true, Messages: []telegramimport.MessageRange{{From: 45, To: 47}}}
	if args.UserID != 1001 || args.Destination != "/Imported" || args.Mode != "copy" || args.ConflictPolicy != "rename" ||
		len(args.Sources) != 1 || args.Sources[0].ChannelID != want.ChannelID || !args.Sources[0].Scan ||
		len(args.Sources[0].Messages) != 1 || args.Sources[0].Messages[0] != want.Messages[0] {
		t.Fatalf("job args = %+v", args)
	}
	page := listJobsPage(t, handler, ctx)
	if len(page.Tasks) != 1 || page.Tasks[0].ID != job.ID {
		t.Fatalf("owner's jobs = %+v, want the import job", page.Tasks)
	}
}
