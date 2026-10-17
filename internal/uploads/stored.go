package uploads

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
)

// StoredPartInput describes a file part that already exists as a Telegram
// document message.
type StoredPartInput struct {
	ChannelID  int64
	MessageID  int64
	PlainSize  int64
	StoredSize int64
}

// PublishHook runs inside the transaction that creates a file from stored
// parts, after the file row exists. An error rolls the whole file back.
type PublishHook func(ctx context.Context, queries *sqlcgen.Queries, file *sqlcgen.File) error

// CreateFromStoredParts publishes a file whose parts already exist in Telegram,
// such as documents imported from a channel. The upload session, its parts and
// the file are created in one transaction. A failure therefore leaves no open
// or aborted session behind: upload cleanup deletes the messages of such
// sessions, and for imported documents those can be the user's own originals.
//
// Parts are numbered from 1 in order and must match the shape implied by
// in.ExpectedSize and in.PartSize. Encrypted files are not supported. hook may
// be nil.
func (s *Service) CreateFromStoredParts(ctx context.Context, in CreateInput, parts []StoredPartInput, hook PublishHook) (*sqlcgen.File, error) {
	if in.Encryption || in.ExpectedSize < 0 || (in.ExpectedSize == 0) != (len(parts) == 0) {
		return nil, ErrInvalidInput
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin stored-part publication: %w", err)
	}
	defer tx.Rollback(ctx)
	q := s.queries.WithTx(tx)

	params, err := s.createSessionParams(ctx, q, in)
	if err != nil {
		return nil, err
	}
	session, err := q.CreateUploadSession(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("create upload session: %w", err)
	}
	uploadID, ok := dbtypes.GoogleUUID(session.ID)
	if !ok {
		return nil, ErrInvalidInput
	}
	for index, part := range parts {
		partNo := int32(index + 1)
		if part.ChannelID == 0 || part.MessageID <= 0 || part.StoredSize < 0 {
			return nil, ErrInvalidInput
		}
		if err := validatePartShape(session, partNo, part.PlainSize); err != nil {
			return nil, err
		}
		if _, err := q.GetChannelForUser(ctx, sqlcgen.GetChannelForUserParams{UserID: in.UserID, ChannelID: part.ChannelID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrInvalidChannel
			}
			return nil, fmt.Errorf("get upload channel: %w", err)
		}
		leaseToken := uuid.New()
		if _, err := q.ClaimUploadPart(ctx, sqlcgen.ClaimUploadPartParams{
			UploadID:       session.ID,
			PartNo:         partNo,
			ChannelID:      part.ChannelID,
			PlainSize:      part.PlainSize,
			LeaseToken:     dbtypes.UUID(leaseToken),
			LeaseExpiresAt: dbtypes.Time(s.now().UTC().Add(s.leaseTTL)),
		}); err != nil {
			return nil, fmt.Errorf("claim stored upload part: %w", err)
		}
		if _, err := q.MarkUploadPartStored(ctx, sqlcgen.MarkUploadPartStoredParams{
			MessageID:  dbtypes.Int8(part.MessageID),
			StoredSize: dbtypes.Int8(part.StoredSize),
			UploadID:   session.ID,
			PartNo:     partNo,
			LeaseToken: dbtypes.UUID(leaseToken),
		}); err != nil {
			return nil, fmt.Errorf("store upload part: %w", err)
		}
	}
	file, replacedID, err := s.completeTx(ctx, tx, in.UserID, uploadID)
	if err != nil {
		return nil, err
	}
	if hook != nil {
		if err := hook(ctx, q, file); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit stored-part publication: %w", err)
	}
	if replacedID != nil && s.catalogInvalidator != nil {
		s.catalogInvalidator.InvalidateFiles(ctx, in.UserID, *replacedID)
	}
	return file, nil
}
