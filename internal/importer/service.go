// Package importer indexes documents that already live in a Teldrive Telegram
// channel. Files forwarded into the channel are catalogued in place: no bytes
// are downloaded, decrypted, or uploaded again.
package importer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

var (
	ErrInvalidInput = errors.New("invalid import request")
	ErrChannel      = errors.New("Telegram channel is not managed by Teldrive")
	ErrNotFound     = errors.New("Telegram document not found")
)

type Service struct {
	queries *sqlcgen.Queries
	storage telegramstore.DocumentMessageReader
	catalog *catalog.Service
}

func NewServiceWithQueries(queries *sqlcgen.Queries, storage telegramstore.DocumentMessageReader, catalogService *catalog.Service) (*Service, error) {
	if queries == nil || storage == nil || catalogService == nil {
		return nil, ErrInvalidInput
	}
	return &Service{queries: queries, storage: storage, catalog: catalogService}, nil
}

type ScanItem struct {
	MessageID int64
	Name      string
	Size      int64
	MimeType  string
	ModTime   time.Time
	Imported  bool
	Link      string
}

type ScanResult struct {
	Items     []ScanItem
	NextID    int64
	Exhausted bool
}

func (s *Service) Scan(ctx context.Context, userID, channelID, beforeID int64, limit int) (ScanResult, error) {
	if userID <= 0 || channelID == 0 || beforeID < 0 {
		return ScanResult{}, ErrInvalidInput
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	if err := s.ensureChannel(ctx, userID, channelID); err != nil {
		return ScanResult{}, err
	}
	page, err := s.storage.ListDocumentMessages(ctx, telegramstore.ListDocumentMessagesRequest{
		UserID: userID, ChannelID: channelID, BeforeID: beforeID, Limit: limit,
	})
	if err != nil {
		return ScanResult{}, fmt.Errorf("scan Telegram channel: %w", err)
	}
	ids := make([]int64, 0, len(page.Messages))
	for _, message := range page.Messages {
		ids = append(ids, message.ID)
	}
	indexed, err := s.indexedMessageIDs(ctx, userID, channelID, ids)
	if err != nil {
		return ScanResult{}, err
	}
	result := ScanResult{Exhausted: page.Exhausted, NextID: page.BeforeID}
	for _, message := range page.Messages {
		result.Items = append(result.Items, ScanItem{
			MessageID: message.ID, Name: message.Name, Size: message.Size, MimeType: message.MimeType,
			ModTime: message.CreatedAt, Imported: indexed[message.ID],
			Link: ChannelMessageLink(channelID, message.ID),
		})
	}
	return result, nil
}

type CreateInput struct {
	UserID     int64
	ChannelID  int64
	ParentID   *uuid.UUID
	MessageIDs []int64
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*sqlcgen.ImportJob, error) {
	if in.UserID <= 0 || in.ChannelID == 0 || len(in.MessageIDs) == 0 || len(in.MessageIDs) > 500 {
		return nil, ErrInvalidInput
	}
	if err := s.ensureChannel(ctx, in.UserID, in.ChannelID); err != nil {
		return nil, err
	}
	if in.ParentID != nil {
		parent, err := s.catalog.Get(ctx, in.UserID, *in.ParentID)
		if err != nil {
			return nil, err
		}
		if parent.Kind != sqlcgen.FileKindFolder || parent.Status != sqlcgen.FileStatusActive {
			return nil, catalog.ErrInvalidParent
		}
	}
	messages, err := s.resolveMessages(ctx, in.UserID, in.ChannelID, in.MessageIDs)
	if err != nil {
		return nil, err
	}
	indexed, err := s.indexedMessageIDs(ctx, in.UserID, in.ChannelID, in.MessageIDs)
	if err != nil {
		return nil, err
	}
	pending := make([]telegramstore.DocumentMessage, 0, len(messages))
	for _, message := range messages {
		if !indexed[message.ID] {
			pending = append(pending, message)
		}
	}
	jobID := uuid.New()
	job, err := s.queries.CreateImportJob(ctx, sqlcgen.CreateImportJobParams{
		ID: dbtypes.UUID(jobID), UserID: in.UserID, ChannelID: in.ChannelID,
		ParentID: dbtypes.OptionalUUID(in.ParentID), Total: int32(len(pending)),
	})
	if err != nil {
		return nil, fmt.Errorf("create import job: %w", err)
	}
	for _, message := range pending {
		if err := s.queries.CreateImportItem(ctx, sqlcgen.CreateImportItemParams{
			JobID: dbtypes.UUID(jobID), MessageID: message.ID, Name: message.Name,
			Size: message.Size, MimeType: dbtypes.OptionalText(optionalString(message.MimeType)),
			ModTime: dbtypes.Time(message.CreatedAt),
		}); err != nil {
			return nil, fmt.Errorf("create import item: %w", err)
		}
	}
	if len(pending) == 0 {
		job, err = s.queries.UpdateImportJobStatus(ctx, sqlcgen.UpdateImportJobStatusParams{
			JobID: dbtypes.UUID(jobID), Status: "completed",
			Error: dbtypes.OptionalText(nil),
		})
		if err != nil {
			return nil, fmt.Errorf("complete empty import job: %w", err)
		}
	}
	return job, nil
}

func (s *Service) GetJob(ctx context.Context, userID int64, jobID uuid.UUID) (*sqlcgen.ImportJob, error) {
	return s.queries.GetImportJob(ctx, sqlcgen.GetImportJobParams{JobID: dbtypes.UUID(jobID), UserID: userID})
}

func (s *Service) resolveMessages(ctx context.Context, userID, channelID int64, ids []int64) ([]telegramstore.DocumentMessage, error) {
	unique := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, ErrInvalidInput
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	messages := make([]telegramstore.DocumentMessage, 0, len(unique))
	for start := 0; start < len(unique); start += 100 {
		end := min(start+100, len(unique))
		found, err := s.storage.GetDocumentMessages(ctx, telegramstore.GetDocumentMessagesRequest{
			UserID: userID, ChannelID: channelID, MessageIDs: unique[start:end],
		})
		if err != nil {
			return nil, fmt.Errorf("resolve Telegram documents: %w", err)
		}
		messages = append(messages, found...)
	}
	if len(messages) == 0 {
		return nil, ErrNotFound
	}
	return messages, nil
}

func (s *Service) indexedMessageIDs(ctx context.Context, userID, channelID int64, ids []int64) (map[int64]bool, error) {
	result := make(map[int64]bool, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	found, err := s.queries.ListIndexedMessageIDs(ctx, sqlcgen.ListIndexedMessageIDsParams{
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

func (s *Service) ensureChannel(ctx context.Context, userID, channelID int64) error {
	if _, err := s.queries.GetChannelForUser(ctx, sqlcgen.GetChannelForUserParams{UserID: userID, ChannelID: channelID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChannel
		}
		return fmt.Errorf("load Telegram channel: %w", err)
	}
	return nil
}

// ChannelMessageLink returns a t.me link for a message in a private channel.
func ChannelMessageLink(channelID, messageID int64) string {
	return fmt.Sprintf("https://t.me/c/%d/%d", channelID, messageID)
}

func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
