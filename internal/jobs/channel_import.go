package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

const ChannelImportKind = "teldrive_channel_import"

type ChannelImportArgs struct {
	JobID     string `json:"job_id"`
	UserID    int64  `json:"user_id"`
	ChannelID int64  `json:"channel_id"`
	ParentID  string `json:"parent_id,omitempty"`
}

func (ChannelImportArgs) Kind() string { return ChannelImportKind }

func (ChannelImportArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: UploadQueue, MaxAttempts: 3, Priority: 2}
}

// ChannelImportWorker indexes documents forwarded into a Teldrive channel. It
// never touches the file body: catalog rows simply reference the existing
// Telegram messages.
type ChannelImportWorker struct {
	river.WorkerDefaults[ChannelImportArgs]
	pool    *pgxpool.Pool
	queries *sqlcgen.Queries
	catalog *catalog.Service
	storage telegramstore.DocumentMessageReader
}

func NewChannelImportWorker(pool *pgxpool.Pool, catalogService *catalog.Service, storage telegramstore.DocumentMessageReader) *ChannelImportWorker {
	return &ChannelImportWorker{pool: pool, queries: sqlcgen.New(pool), catalog: catalogService, storage: storage}
}

func (w *ChannelImportWorker) Timeout(*river.Job[ChannelImportArgs]) time.Duration {
	return 30 * time.Minute
}

func (w *ChannelImportWorker) Work(ctx context.Context, job *river.Job[ChannelImportArgs]) error {
	if w == nil || w.pool == nil || w.catalog == nil || w.storage == nil {
		return errors.New("channel import worker is not configured")
	}
	jobID, err := uuid.Parse(job.Args.JobID)
	if err != nil {
		return fmt.Errorf("invalid import job id: %w", err)
	}
	if _, err := w.queries.UpdateImportJobStatus(ctx, sqlcgen.UpdateImportJobStatusParams{
		JobID: dbtypes.UUID(jobID), Status: "running", Error: dbtypes.OptionalText(nil),
	}); err != nil {
		return fmt.Errorf("mark import job running: %w", err)
	}
	items, err := w.queries.ListPendingImportItems(ctx, dbtypes.UUID(jobID))
	if err != nil {
		return fmt.Errorf("list import items: %w", err)
	}
	parentID, err := parseImportParentID(job.Args.ParentID)
	if err != nil {
		return err
	}
	indexed, err := w.indexedIDs(ctx, job.Args.UserID, job.Args.ChannelID, items)
	if err != nil {
		return err
	}
	var imported, skipped, failed int32
	for _, item := range items {
		if indexed[item.MessageID] {
			skipped++
			_ = w.queries.MarkImportItemFailed(ctx, sqlcgen.MarkImportItemFailedParams{
				JobID: dbtypes.UUID(jobID), MessageID: item.MessageID, Status: "skipped",
				Error: dbtypes.OptionalText(optionalStringPtr("already indexed")),
			})
			continue
		}
		fileID, err := w.importItem(ctx, job.Args.UserID, job.Args.ChannelID, parentID, item)
		if err != nil {
			failed++
			slog.WarnContext(ctx, "jobs.channel_import_item_failed", "job_id", jobID, "message_id", item.MessageID, "error", err)
			_ = w.queries.MarkImportItemFailed(ctx, sqlcgen.MarkImportItemFailedParams{
				JobID: dbtypes.UUID(jobID), MessageID: item.MessageID, Status: "failed",
				Error: dbtypes.OptionalText(optionalStringPtr(err.Error())),
			})
		} else {
			imported++
			_ = w.queries.MarkImportItemImported(ctx, sqlcgen.MarkImportItemImportedParams{
				JobID: dbtypes.UUID(jobID), MessageID: item.MessageID, FileID: dbtypes.UUID(fileID),
			})
			w.catalog.InvalidateFiles(ctx, job.Args.UserID, fileID)
		}
		if imported+skipped+failed > 0 && (imported+skipped+failed)%25 == 0 {
			_, _ = w.queries.IncrementImportJobProgress(ctx, sqlcgen.IncrementImportJobProgressParams{
				JobID: dbtypes.UUID(jobID), Imported: imported, Skipped: skipped, Failed: failed,
			})
			imported, skipped, failed = 0, 0, 0
		}
	}
	if imported+skipped+failed > 0 {
		if _, err := w.queries.IncrementImportJobProgress(ctx, sqlcgen.IncrementImportJobProgressParams{
			JobID: dbtypes.UUID(jobID), Imported: imported, Skipped: skipped, Failed: failed,
		}); err != nil {
			return fmt.Errorf("update import progress: %w", err)
		}
	}
	status := "completed"
	if _, err := w.queries.UpdateImportJobStatus(ctx, sqlcgen.UpdateImportJobStatusParams{
		JobID: dbtypes.UUID(jobID), Status: status, Error: dbtypes.OptionalText(nil),
	}); err != nil {
		return fmt.Errorf("complete import job: %w", err)
	}
	return nil
}

func (w *ChannelImportWorker) importItem(ctx context.Context, userID, channelID int64, parentID *uuid.UUID, item *sqlcgen.ImportItem) (uuid.UUID, error) {
	fileID := uuid.New()
	name := item.Name
	existing, err := w.queries.ListActiveNames(ctx, sqlcgen.ListActiveNamesParams{
		UserID: userID, ParentID: dbtypes.OptionalUUID(parentID), ExcludeID: dbtypes.OptionalUUID(nil),
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("list destination names: %w", err)
	}
	used := make(map[string]struct{}, len(existing))
	for _, value := range existing {
		used[value] = struct{}{}
	}
	base, extension := splitName(item.Name)
	for attempt := 0; attempt < 10000; attempt++ {
		candidate := name
		if attempt > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", base, attempt, extension)
		}
		if _, exists := used[candidate]; exists {
			continue
		}
		mimeType := dbtypes.OptionalText(optionalStringPtr(item.MimeType.String))
		file, err := w.queries.InsertImportedFile(ctx, sqlcgen.InsertImportedFileParams{
			ID: dbtypes.UUID(fileID), UserID: userID, ParentID: dbtypes.OptionalUUID(parentID),
			Name: candidate, MimeType: mimeType, Size: dbtypes.Int8(item.Size), ModTime: item.ModTime,
		})
		if err != nil {
			if isUniqueViolation(err) {
				continue
			}
			return uuid.Nil, fmt.Errorf("create imported file: %w", err)
		}
		id, ok := dbtypes.GoogleUUID(file.ID)
		if !ok {
			return uuid.Nil, errors.New("imported file has invalid ID")
		}
		fileID = id
		break
	}
	if err := w.queries.InsertImportedFilePart(ctx, sqlcgen.InsertImportedFilePartParams{
		FileID: dbtypes.UUID(fileID), PartNo: 1, ChannelID: channelID, MessageID: item.MessageID,
		PlainSize: dbtypes.Int8(item.Size), StoredSize: dbtypes.Int8(item.Size),
	}); err != nil {
		return uuid.Nil, fmt.Errorf("create imported file part: %w", err)
	}
	return fileID, nil
}

func (w *ChannelImportWorker) indexedIDs(ctx context.Context, userID, channelID int64, items []*sqlcgen.ImportItem) (map[int64]bool, error) {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.MessageID)
	}
	result := make(map[int64]bool, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	found, err := w.queries.ListIndexedMessageIDs(ctx, sqlcgen.ListIndexedMessageIDsParams{
		UserID: userID, ChannelID: channelID, MessageIds: ids,
	})
	if err != nil {
		return nil, fmt.Errorf("check indexed messages: %w", err)
	}
	for _, id := range found {
		result[id] = true
	}
	return result, nil
}

func splitName(name string) (string, string) {
	index := -1
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			index = i
			break
		}
	}
	if index <= 0 || index == len(name)-1 {
		return name, ""
	}
	return name[:index], name[index:]
}

func parseImportParentID(value string) (*uuid.UUID, error) {
	if value == "" {
		return nil, nil
	}
	id, err := uuid.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid parent id: %w", err)
	}
	return &id, nil
}

func optionalStringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
