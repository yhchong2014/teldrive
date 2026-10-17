package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/telegramimport"
)

const TelegramImportKind = "teldrive_telegram_import"

var errInvalidTelegramImport = errors.New("invalid Telegram import")

// TelegramImportArgs imports documents that already exist in Telegram.
// user_id is part of the arguments so that the job is listed for its owner.
type TelegramImportArgs struct {
	UserID         int64                   `json:"user_id"`
	Destination    string                  `json:"destination"`
	Sources        []telegramimport.Source `json:"sources"`
	Mode           string                  `json:"mode,omitempty"`
	ConflictPolicy string                  `json:"conflict_policy,omitempty"`
}

func (TelegramImportArgs) Kind() string { return TelegramImportKind }

// InsertOpts allows retries: an import skips what earlier attempts imported.
func (TelegramImportArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: UploadQueue, MaxAttempts: 3}
}

type TelegramImportWorker struct {
	river.WorkerDefaults[TelegramImportArgs]
	catalog  *catalog.Service
	importer *telegramimport.Service
}

func NewTelegramImportWorker(catalogService *catalog.Service, importer *telegramimport.Service) *TelegramImportWorker {
	return &TelegramImportWorker{catalog: catalogService, importer: importer}
}

func (w *TelegramImportWorker) Timeout(*river.Job[TelegramImportArgs]) time.Duration {
	return 24 * time.Hour
}

func (w *TelegramImportWorker) Work(ctx context.Context, job *river.Job[TelegramImportArgs]) error {
	if w == nil || w.catalog == nil || w.importer == nil {
		return ErrRuntimeNotConfigured
	}
	if job.Args.UserID <= 0 || len(job.Args.Sources) == 0 {
		return river.JobCancel(errInvalidTelegramImport)
	}
	mode, err := telegramimport.ParseMode(job.Args.Mode)
	if err != nil {
		return river.JobCancel(err)
	}
	policy := sqlcgen.NameConflictPolicy(job.Args.ConflictPolicy)
	switch policy {
	case "":
		policy = sqlcgen.NameConflictPolicyRename
	case sqlcgen.NameConflictPolicyFail, sqlcgen.NameConflictPolicyRename, sqlcgen.NameConflictPolicyReplace:
	default:
		return river.JobCancel(fmt.Errorf("%w: conflict policy %q", errInvalidTelegramImport, job.Args.ConflictPolicy))
	}
	parentID, err := resolveImportDestination(ctx, w.catalog, job.Args.UserID, job.Args.Destination)
	if err != nil {
		return err
	}
	client, _ := river.ClientFromContextSafely[pgx.Tx](ctx)
	report := func(ctx context.Context, progress telegramimport.Progress) {
		if client == nil || job.JobRow == nil {
			return
		}
		_, _ = client.JobUpdate(ctx, job.ID, &river.JobUpdateParams{Output: progress})
	}
	progress, runErr := w.importer.Run(ctx, telegramimport.Options{
		UserID: job.Args.UserID, ParentID: parentID, Mode: mode, ConflictPolicy: policy,
	}, job.Args.Sources, report)
	if client != nil && job.JobRow != nil {
		if runErr != nil {
			report(context.WithoutCancel(ctx), progress)
		} else if err := river.RecordOutput(ctx, progress); err != nil {
			return err
		}
	}
	return runErr
}

// resolveImportDestination resolves a folder UUID or an absolute drive path,
// creating missing folders of the path.
func resolveImportDestination(ctx context.Context, catalogService *catalog.Service, userID int64, destination string) (*uuid.UUID, error) {
	destination = strings.TrimSpace(destination)
	if destination == "" || destination == "/" {
		return nil, nil
	}
	if id, err := uuid.Parse(destination); err == nil {
		folder, err := catalogService.Get(ctx, userID, id)
		if err != nil {
			return nil, fmt.Errorf("resolve import destination: %w", err)
		}
		if folder.Kind != sqlcgen.FileKindFolder || folder.Status != sqlcgen.FileStatusActive {
			return nil, river.JobCancel(fmt.Errorf("%w: destination is not an active folder", errInvalidTelegramImport))
		}
		return &id, nil
	}
	if !strings.HasPrefix(destination, "/") {
		return nil, river.JobCancel(fmt.Errorf("%w: destination path must be absolute", errInvalidTelegramImport))
	}
	id, err := catalogService.EnsureFolderPath(ctx, userID, nil, destination)
	if err != nil {
		return nil, fmt.Errorf("create import destination: %w", err)
	}
	return id, nil
}
