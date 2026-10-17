package telegramimport

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

const (
	// pageSize is the number of messages read per Telegram request.
	pageSize = 100
	// maxReportedErrors bounds the failure messages kept in the progress.
	maxReportedErrors = 20
	// reportInterval throttles progress reports.
	reportInterval = 2 * time.Second
	// maxFloodWait is the longest Telegram flood wait an import sits out.
	// The client middleware absorbs short waits; copying a large channel can
	// earn longer ones.
	maxFloodWait = 30 * time.Minute
	// maxFloodRetries bounds the flood waits one Telegram call sits out.
	maxFloodRetries = 5
)

var (
	ErrUnsupported   = errors.New("Telegram storage does not support imports")
	ErrInvalidOption = errors.New("invalid Telegram import option")

	errAlreadyImported = errors.New("document is already in TelDrive")
)

// Mode selects how documents enter TelDrive.
type Mode string

const (
	// ModeAuto references documents of channels registered in TelDrive and
	// copies documents of other channels.
	ModeAuto Mode = "auto"
	// ModeReference points TelDrive files at the original messages. The source
	// channel must be one of the user's TelDrive channels.
	ModeReference Mode = "reference"
	// ModeCopy sends each document to the selected storage channel as a new
	// message, leaving the source message untouched.
	ModeCopy Mode = "copy"
)

// ParseMode validates a mode; empty means ModeAuto.
func ParseMode(raw string) (Mode, error) {
	switch mode := Mode(strings.ToLower(strings.TrimSpace(raw))); mode {
	case "":
		return ModeAuto, nil
	case ModeAuto, ModeReference, ModeCopy:
		return mode, nil
	default:
		return "", fmt.Errorf("%w: mode %q", ErrInvalidOption, raw)
	}
}

// ChannelResolver chooses the storage channel that copies are sent to.
type ChannelResolver interface {
	Resolve(ctx context.Context, userID, requestedChannelID int64) (int64, error)
}

// Options configures one import run.
type Options struct {
	UserID         int64
	ParentID       *uuid.UUID
	Mode           Mode
	ConflictPolicy sqlcgen.NameConflictPolicy
}

// Progress describes an import run. Jobs store it as their output.
type Progress struct {
	Stage         string    `json:"stage"`
	Channel       string    `json:"channel,omitempty"`
	Scanned       int       `json:"scanned"`
	Imported      int       `json:"imported"`
	Skipped       int       `json:"skipped"`
	Failed        int       `json:"failed"`
	ImportedBytes int64     `json:"importedBytes"`
	Errors        []string  `json:"errors,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Service imports Telegram documents into the catalog.
type Service struct {
	queries  *sqlcgen.Queries
	uploads  *uploads.Service
	channels ChannelResolver
	storage  telegramstore.Storage
	importer telegramstore.DocumentImporter
	now      func() time.Time
	sleep    func(context.Context, time.Duration) bool
}

func NewService(pool *pgxpool.Pool, uploadService *uploads.Service, channelResolver ChannelResolver, storage telegramstore.Storage) (*Service, error) {
	if pool == nil || uploadService == nil || channelResolver == nil || storage == nil {
		return nil, ErrInvalidOption
	}
	importer, ok := storage.(telegramstore.DocumentImporter)
	if !ok {
		return nil, ErrUnsupported
	}
	return &Service{
		queries: sqlcgen.New(pool), uploads: uploadService, channels: channelResolver,
		storage: storage, importer: importer, now: time.Now, sleep: sleepContext,
	}, nil
}

// sleepContext waits for d and reports whether ctx is still live.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// withFloodRetry runs call and sits out Telegram flood waits of up to
// maxFloodWait before retrying it.
func withFloodRetry[T any](ctx context.Context, sleep func(context.Context, time.Duration) bool, call func() (T, error)) (T, error) {
	result, err := call()
	for attempt := 0; err != nil && attempt < maxFloodRetries; attempt++ {
		wait, ok := tgerr.AsFloodWait(err)
		if !ok || wait > maxFloodWait {
			break
		}
		slog.InfoContext(ctx, "telegram_import.flood_wait", "wait", wait)
		if !sleep(ctx, wait+time.Second) {
			return result, ctx.Err()
		}
		result, err = call()
	}
	return result, err
}

// Run imports every source. Failures of single documents and inaccessible
// channels are counted in the progress, and the run goes on; Run returns an
// error only when it cannot continue, for example because ctx ended or the
// Telegram session failed. Repeating a run imports only what is still missing.
// report, which may be nil, receives throttled progress snapshots.
func (s *Service) Run(ctx context.Context, opts Options, sources []Source, report func(context.Context, Progress)) (Progress, error) {
	now := s.now().UTC()
	run := &importRun{
		service: s, opts: opts, report: report,
		progress: Progress{Stage: "importing", StartedAt: now, UpdatedAt: now},
	}
	if opts.UserID <= 0 || len(sources) == 0 {
		run.progress.Stage = "failed"
		return run.progress, ErrInvalidOption
	}
	if run.opts.Mode == "" {
		run.opts.Mode = ModeAuto
	}
	if run.opts.ConflictPolicy == "" {
		run.opts.ConflictPolicy = sqlcgen.NameConflictPolicyRename
	}
	run.publish(ctx, true)
	err := s.importer.RunImport(ctx, opts.UserID, func(ctx context.Context, session telegramstore.ImportSession) error {
		run.session = session
		for _, source := range sources {
			if err := run.importSource(ctx, source); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		run.progress.Stage = "failed"
		run.recordError(fmt.Sprintf("import stopped: %v", err))
	} else {
		run.progress.Stage = "completed"
	}
	run.progress.UpdatedAt = s.now().UTC()
	return run.snapshot(), err
}

type importRun struct {
	service    *Service
	opts       Options
	session    telegramstore.ImportSession
	report     func(context.Context, Progress)
	progress   Progress
	reportedAt time.Time
}

type importTarget struct {
	channel     telegramstore.ImportChannel
	label       string
	mode        Mode
	registered  bool
	destination int64
}

func (r *importRun) importSource(ctx context.Context, source Source) error {
	label := source.Label()
	r.progress.Channel = label
	channel, err := r.session.ResolveChannel(ctx, source.Ref())
	if err != nil {
		if errors.Is(err, telegramstore.ErrInvalidChannel) {
			r.progress.Failed++
			r.recordError(fmt.Sprintf("%s: %v", label, err))
			r.publish(ctx, true)
			return nil
		}
		return err
	}
	if channel.Title != "" {
		label = channel.Title
		r.progress.Channel = label
	}
	registered, err := r.registered(ctx, channel.ID)
	if err != nil {
		return err
	}
	target := &importTarget{channel: channel, label: label, mode: r.opts.Mode, registered: registered}
	if target.mode == ModeAuto {
		target.mode = ModeCopy
		if registered {
			target.mode = ModeReference
		}
	}
	if target.mode == ModeReference && !registered {
		r.progress.Failed++
		r.recordError(fmt.Sprintf("%s: reference mode needs one of your TelDrive channels; use copy mode for other channels", label))
		r.publish(ctx, true)
		return nil
	}

	if source.Scan {
		var beforeID int64
		for {
			page, err := withFloodRetry(ctx, r.service.sleep, func() (telegramstore.ImportHistoryPage, error) {
				return r.session.ListDocuments(ctx, channel, beforeID, pageSize)
			})
			if err != nil {
				return err
			}
			if err := r.importDocuments(ctx, target, page.Documents); err != nil {
				return err
			}
			if page.Exhausted || page.NextBeforeID <= 0 {
				break
			}
			beforeID = page.NextBeforeID
		}
	}
	if len(source.Messages) > 0 {
		err := expand(source.Messages, pageSize, func(ids []int64) error {
			documents, err := withFloodRetry(ctx, r.service.sleep, func() ([]telegramstore.ImportDocument, error) {
				return r.session.GetDocuments(ctx, channel, ids)
			})
			if err != nil {
				return err
			}
			// Deleted messages and messages without a document.
			r.progress.Scanned += len(ids) - len(documents)
			r.progress.Skipped += len(ids) - len(documents)
			return r.importDocuments(ctx, target, documents)
		})
		if err != nil {
			return err
		}
	}
	r.publish(ctx, true)
	return nil
}

// registered reports whether the channel is one of the user's TelDrive
// channels.
func (r *importRun) registered(ctx context.Context, channelID int64) (bool, error) {
	_, err := r.service.queries.GetChannelForUser(ctx, sqlcgen.GetChannelForUserParams{UserID: r.opts.UserID, ChannelID: channelID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get TelDrive channel: %w", err)
	}
	return true, nil
}

func (r *importRun) importDocuments(ctx context.Context, target *importTarget, documents []telegramstore.ImportDocument) error {
	if len(documents) == 0 {
		return nil
	}
	// Import oldest first so that renamed duplicates number in posting order.
	documents = slices.Clone(documents)
	slices.SortFunc(documents, func(a, b telegramstore.ImportDocument) int { return cmp.Compare(a.MessageID, b.MessageID) })
	r.progress.Scanned += len(documents)

	ids := make([]int64, len(documents))
	for i, document := range documents {
		ids[i] = document.MessageID
	}
	known, err := r.service.queries.ListImportedTelegramMessages(ctx, sqlcgen.ListImportedTelegramMessagesParams{
		UserID: r.opts.UserID, SourceChannelID: target.channel.ID, MessageIds: ids,
	})
	if err != nil {
		return fmt.Errorf("list imported Telegram messages: %w", err)
	}
	if target.registered {
		// Messages of TelDrive channels may already belong to uploaded files.
		referenced, err := r.service.queries.ListReferencedMessageIDs(ctx, sqlcgen.ListReferencedMessageIDsParams{
			TargetChannelID: target.channel.ID, MessageIds: ids,
		})
		if err != nil {
			return fmt.Errorf("list referenced Telegram messages: %w", err)
		}
		known = append(known, referenced...)
	}
	for _, document := range documents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if document.Size <= 0 || slices.Contains(known, document.MessageID) {
			r.progress.Skipped++
			continue
		}
		err := r.importDocument(ctx, target, document)
		switch {
		case err == nil:
			r.progress.Imported++
			r.progress.ImportedBytes += document.Size
		case errors.Is(err, errAlreadyImported):
			r.progress.Skipped++
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			r.progress.Failed++
			message := fmt.Sprintf("%s #%d (%s): %v", target.label, document.MessageID, DocumentName(document), err)
			r.recordError(message)
			slog.WarnContext(ctx, "telegram_import.document_failed", "user_id", r.opts.UserID,
				"channel_id", target.channel.ID, "message_id", document.MessageID, "error", err)
		}
		r.publish(ctx, false)
	}
	return nil
}

func (r *importRun) importDocument(ctx context.Context, target *importTarget, document telegramstore.ImportDocument) error {
	part := uploads.StoredPartInput{
		ChannelID: target.channel.ID, MessageID: document.MessageID,
		PlainSize: document.Size, StoredSize: document.Size,
	}
	if target.mode == ModeCopy {
		if target.destination == 0 {
			destination, err := r.service.channels.Resolve(ctx, r.opts.UserID, 0)
			if err != nil {
				return fmt.Errorf("resolve storage channel: %w", err)
			}
			target.destination = destination
		}
		stored, err := withFloodRetry(ctx, r.service.sleep, func() (telegramstore.StoredPart, error) {
			return r.session.CopyDocument(ctx, target.channel, document, target.destination)
		})
		if err != nil {
			if stored.MessageID > 0 {
				r.discard(ctx, stored.ChannelID, stored.MessageID)
			}
			return err
		}
		part.ChannelID, part.MessageID, part.StoredSize = stored.ChannelID, stored.MessageID, stored.Size
	}

	var mimeType *string
	if value := strings.TrimSpace(document.MIMEType); value != "" {
		mimeType = &value
	}
	input := uploads.CreateInput{
		UserID: r.opts.UserID, ParentID: r.opts.ParentID, Name: DocumentName(document),
		ExpectedSize: document.Size, PartSize: document.Size, MIMEType: mimeType,
		ModTime: document.Date, ConflictPolicy: r.opts.ConflictPolicy,
	}
	_, err := r.service.uploads.CreateFromStoredParts(ctx, input, []uploads.StoredPartInput{part},
		func(ctx context.Context, queries *sqlcgen.Queries, file *sqlcgen.File) error {
			return queries.RecordTelegramImport(ctx, sqlcgen.RecordTelegramImportParams{
				UserID: r.opts.UserID, SourceChannelID: target.channel.ID, SourceMessageID: document.MessageID, FileID: file.ID,
			})
		})
	if err == nil {
		return nil
	}
	if target.mode == ModeCopy {
		r.discard(ctx, part.ChannelID, part.MessageID)
	} else if isUniqueViolation(err) {
		// Another import or upload referenced the message meanwhile.
		return errAlreadyImported
	}
	return err
}

// discard deletes a copy that did not become a file. Copies that cannot be
// deleted now are unreferenced and fall to the orphan cleanup later.
func (r *importRun) discard(ctx context.Context, channelID, messageID int64) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := r.service.storage.DeleteMessages(cleanupCtx, r.opts.UserID, channelID, []int64{messageID}); err != nil {
		slog.WarnContext(ctx, "telegram_import.discard_failed", "user_id", r.opts.UserID,
			"channel_id", channelID, "message_id", messageID, "error", err)
	}
}

func (r *importRun) recordError(message string) {
	if len(r.progress.Errors) < maxReportedErrors {
		r.progress.Errors = append(r.progress.Errors, message)
	}
}

func (r *importRun) publish(ctx context.Context, force bool) {
	now := r.service.now()
	if r.report == nil || (!force && now.Sub(r.reportedAt) < reportInterval) {
		return
	}
	r.reportedAt = now
	r.progress.UpdatedAt = now.UTC()
	r.report(ctx, r.snapshot())
}

func (r *importRun) snapshot() Progress {
	progress := r.progress
	progress.Errors = slices.Clone(r.progress.Errors)
	return progress
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
