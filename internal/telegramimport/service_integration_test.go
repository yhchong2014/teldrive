//go:build integration

package telegramimport_test

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/channels"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/localtelegram"
	"github.com/tgdrive/teldrive/v2/internal/telegramimport"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
	testpostgres "github.com/tgdrive/teldrive/v2/internal/testutil/postgres"
	"github.com/tgdrive/teldrive/v2/internal/transfer"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

const importUser int64 = 1001

type importFixture struct {
	pool     *pgxpool.Pool
	storage  *telegramstore.GotdStorage
	catalog  *catalog.Service
	service  *telegramimport.Service
	source   telegramstore.Channel
	store    telegramstore.Channel
	payloads map[int64][]byte
}

func newImportFixture(t *testing.T) *importFixture {
	t.Helper()
	ctx := context.Background()
	db := testpostgres.New(t)
	server, err := localtelegram.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := localtelegram.NewRunner(server)
	if err != nil {
		t.Fatal(err)
	}
	storage := telegramstore.NewGotdStorage(runner, nil)
	source, err := storage.CreateChannel(ctx, importUser, "movies")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.CreateChannel(ctx, importUser, "teldrive-storage")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES ($1)", importUser); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "INSERT INTO channels (channel_id, user_id, name, selected) VALUES ($1, $2, 'storage', true)", store.ID, importUser); err != nil {
		t.Fatal(err)
	}
	uploadService := uploads.NewService(db.Pool, 7*24*time.Hour)
	catalogService := catalog.NewService(db.Pool, nil)
	channelService := channels.NewService(db.Pool, channels.TelegramCreator{Storage: storage}, channels.Config{PartLimit: 1000, NamePrefix: "teldrive"})
	service, err := telegramimport.NewService(db.Pool, uploadService, channelService, storage)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &importFixture{
		pool: db.Pool, storage: storage, catalog: catalogService, service: service,
		source: source, store: store, payloads: make(map[int64][]byte),
	}
	for i, name := range []string{"first.mkv", "second.mp4", "third.bin"} {
		payload := bytes.Repeat([]byte{byte('a' + i)}, 70_000+i*1000)
		stored, err := storage.Upload(ctx, telegramstore.UploadRequest{
			UserID: importUser, ChannelID: source.ID, Name: name,
			Reader: bytes.NewReader(payload), Size: int64(len(payload)), Threads: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		fixture.payloads[stored.MessageID] = payload
	}
	return fixture
}

func (f *importFixture) messageIDs() []int64 {
	ids := make([]int64, 0, len(f.payloads))
	for id := range f.payloads {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (f *importFixture) run(t *testing.T, opts telegramimport.Options, sources ...telegramimport.Source) telegramimport.Progress {
	t.Helper()
	if opts.UserID == 0 {
		opts.UserID = importUser
	}
	var reports int
	progress, err := f.service.Run(context.Background(), opts, sources, func(context.Context, telegramimport.Progress) { reports++ })
	if err != nil {
		t.Fatalf("Run() error = %v (progress %+v)", err, progress)
	}
	if progress.Stage != "completed" || reports == 0 {
		t.Fatalf("progress = %+v after %d reports", progress, reports)
	}
	return progress
}

func (f *importFixture) files(t *testing.T) []*sqlcgen.File {
	t.Helper()
	files, err := f.catalog.List(context.Background(), catalog.ListInput{UserID: importUser, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func (f *importFixture) parts(t *testing.T, file *sqlcgen.File) []*sqlcgen.FilePart {
	t.Helper()
	id, _ := dbtypes.GoogleUUID(file.ID)
	parts, err := f.catalog.Parts(context.Background(), importUser, id)
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

func (f *importFixture) download(t *testing.T, file *sqlcgen.File) []byte {
	t.Helper()
	id, _ := dbtypes.GoogleUUID(file.ID)
	download, err := transfer.NewDownloader(f.catalog, f.storage, nil).Open(context.Background(), transfer.DownloadRequest{
		UserID: importUser, FileID: id, Length: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer download.Reader.Close()
	content, err := io.ReadAll(download.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func (f *importFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestChannelScanCopiesForeignDocumentsOnce(t *testing.T) {
	f := newImportFixture(t)
	progress := f.run(t, telegramimport.Options{}, telegramimport.Source{ChannelID: f.source.ID, Scan: true})
	if progress.Imported != 3 || progress.Skipped != 0 || progress.Failed != 0 || progress.Channel != "movies" {
		t.Fatalf("first scan progress = %+v", progress)
	}
	files := f.files(t)
	if len(files) != 3 {
		t.Fatalf("files = %d, want 3", len(files))
	}
	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, file.Name)
		parts := f.parts(t, file)
		// The source channel is not a TelDrive channel: documents are copied
		// into the selected storage channel.
		if len(parts) != 1 || parts[0].ChannelID != f.store.ID {
			t.Fatalf("%s parts = %+v, want one part in storage channel %d", file.Name, parts, f.store.ID)
		}
		if _, isSource := f.payloads[parts[0].MessageID]; isSource {
			t.Fatalf("%s references a source message ID", file.Name)
		}
		var want []byte
		for _, payload := range f.payloads {
			if int64(len(payload)) == file.Size.Int64 {
				want = payload
			}
		}
		if got := f.download(t, file); !bytes.Equal(got, want) {
			t.Fatalf("%s downloaded %d bytes that differ from the source document", file.Name, len(got))
		}
	}
	slices.Sort(names)
	if want := []string{"first.mkv", "second.mp4", "third.bin"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	// Source messages are untouched.
	for id, payload := range f.payloads {
		reader, err := f.storage.OpenRange(context.Background(), telegramstore.RangeRequest{UserID: importUser, ChannelID: f.source.ID, MessageID: id, Length: -1})
		if err != nil {
			t.Fatalf("source message %d: %v", id, err)
		}
		content, _ := io.ReadAll(reader)
		_ = reader.Close()
		if !bytes.Equal(content, payload) {
			t.Fatalf("source message %d changed", id)
		}
	}

	again := f.run(t, telegramimport.Options{}, telegramimport.Source{ChannelID: f.source.ID, Scan: true})
	if again.Imported != 0 || again.Skipped != 3 || again.Failed != 0 {
		t.Fatalf("repeated scan progress = %+v, want everything skipped", again)
	}
	if got := len(f.files(t)); got != 3 {
		t.Fatalf("files after repeated scan = %d, want 3", got)
	}
	if got := f.count(t, "SELECT count(*) FROM telegram_import_sources WHERE user_id = $1 AND source_channel_id = $2", importUser, f.source.ID); got != 3 {
		t.Fatalf("recorded import sources = %d, want 3", got)
	}
}

func TestMessageImportReferencesTelDriveChannelDocuments(t *testing.T) {
	f := newImportFixture(t)
	ctx := context.Background()
	// Registering the source channel makes auto mode reference its documents.
	if _, err := f.pool.Exec(ctx, "INSERT INTO channels (channel_id, user_id, name) VALUES ($1, $2, 'movies')", f.source.ID, importUser); err != nil {
		t.Fatal(err)
	}
	ids := f.messageIDs()
	folder, err := f.catalog.CreateFolder(ctx, catalog.CreateFolderInput{UserID: importUser, Name: "Imported"})
	if err != nil {
		t.Fatal(err)
	}
	parentID, _ := dbtypes.GoogleUUID(folder.ID)
	progress := f.run(t, telegramimport.Options{ParentID: &parentID}, telegramimport.Source{
		ChannelID: f.source.ID,
		// The second document plus a message ID that does not exist.
		Messages: []telegramimport.MessageRange{{From: ids[1], To: ids[1]}, {From: 9999, To: 9999}},
	})
	if progress.Imported != 1 || progress.Skipped != 1 || progress.Failed != 0 {
		t.Fatalf("progress = %+v", progress)
	}
	files, err := f.catalog.List(ctx, catalog.ListInput{UserID: importUser, ParentID: &parentID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "second.mp4" {
		t.Fatalf("imported files = %+v", files)
	}
	parts := f.parts(t, files[0])
	if len(parts) != 1 || parts[0].ChannelID != f.source.ID || parts[0].MessageID != ids[1] {
		t.Fatalf("parts = %+v, want a reference to message %d of the source channel", parts, ids[1])
	}
	if got := f.download(t, files[0]); !bytes.Equal(got, f.payloads[ids[1]]) {
		t.Fatal("referenced document content differs")
	}

	// A channel scan now imports only the two remaining documents.
	scan := f.run(t, telegramimport.Options{ParentID: &parentID}, telegramimport.Source{ChannelID: f.source.ID, Scan: true})
	if scan.Imported != 2 || scan.Skipped != 1 {
		t.Fatalf("scan progress = %+v", scan)
	}
}

func TestFailedReferenceImportLeavesNoSessionForCleanup(t *testing.T) {
	f := newImportFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, "INSERT INTO channels (channel_id, user_id, name) VALUES ($1, $2, 'movies')", f.source.ID, importUser); err != nil {
		t.Fatal(err)
	}
	if _, err := f.catalog.CreateFolder(ctx, catalog.CreateFolderInput{UserID: importUser, Name: "first.mkv"}); err != nil {
		t.Fatal(err)
	}
	ids := f.messageIDs()
	progress := f.run(t, telegramimport.Options{ConflictPolicy: sqlcgen.NameConflictPolicyFail}, telegramimport.Source{
		ChannelID: f.source.ID, Messages: []telegramimport.MessageRange{{From: ids[0], To: ids[0]}},
	})
	if progress.Failed != 1 || progress.Imported != 0 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0], "first.mkv") {
		t.Fatalf("progress = %+v, want one name-conflict failure", progress)
	}
	// Upload cleanup deletes the messages of leftover sessions; a failed
	// reference import must not leave one pointing at the original message.
	if got := f.count(t, "SELECT count(*) FROM upload_sessions"); got != 0 {
		t.Fatalf("upload sessions after failed import = %d, want 0", got)
	}
	if got := f.count(t, "SELECT count(*) FROM upload_parts"); got != 0 {
		t.Fatalf("upload parts after failed import = %d, want 0", got)
	}
}

func TestReferenceModeRequiresTelDriveChannel(t *testing.T) {
	f := newImportFixture(t)
	progress := f.run(t, telegramimport.Options{Mode: telegramimport.ModeReference}, telegramimport.Source{ChannelID: f.source.ID, Scan: true})
	if progress.Failed != 1 || progress.Imported != 0 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0], "copy mode") {
		t.Fatalf("progress = %+v", progress)
	}
}

func TestUnknownChannelsAreReportedAndSkipped(t *testing.T) {
	f := newImportFixture(t)
	progress := f.run(t, telegramimport.Options{Mode: telegramimport.ModeCopy},
		telegramimport.Source{Username: "nobody_here", Scan: true},
		telegramimport.Source{ChannelID: f.source.ID, Messages: []telegramimport.MessageRange{{From: f.messageIDs()[2], To: f.messageIDs()[2]}}},
	)
	if progress.Failed != 1 || progress.Imported != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0], "@nobody_here") {
		t.Fatalf("progress = %+v", progress)
	}
	if files := f.files(t); len(files) != 1 || files[0].Name != "third.bin" {
		t.Fatalf("files = %+v", files)
	}
	_ = uuid.Nil
}
