package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/importer"
	"github.com/tgdrive/teldrive/v2/internal/streamtoken"
)

// ChannelImporter indexes documents already forwarded into a Telegram channel.
type ChannelImporter interface {
	Scan(ctx context.Context, userID, channelID, beforeID int64, limit int) (importer.ScanResult, error)
	Create(ctx context.Context, in importer.CreateInput) (*sqlcgen.ImportJob, error)
}

// WebDAVCredentialStore manages the dedicated WebDAV login shown in Settings.
type WebDAVCredentialStore interface {
	Set(ctx context.Context, userID int64, username, password string) (*sqlcgen.WebdavCredential, error)
	Delete(ctx context.Context, userID int64) error
	GetByUser(ctx context.Context, userID int64) (*sqlcgen.WebdavCredential, error)
}

// DownloadAccountSource reports the Telegram accounts that may serve a
// download for a user. Bot-aware deployments return the eligible bot IDs; a
// user-only deployment returns the user ID.
type DownloadAccountSource interface {
	DownloadAccounts(ctx context.Context, userID int64) ([]int64, error)
}

// CreateStreamToken issues a short-lived signed URL that a media player can
// open directly. The token is scoped to this file and pinned to one Telegram
// account, so the player's probing and seeking requests keep reusing the same
// warm client instead of racing through the bot rotation.
func (h *Handler) CreateStreamToken(ctx context.Context, params gen.CreateStreamTokenParams) (gen.CreateStreamTokenRes, error) {
	if h == nil || h.StreamTokens == nil || h.Catalog == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	fileID := googleUUID(params.FileId)
	access, err := h.resolveAuthenticatedFileAccess(ctx, fileID, false)
	if err != nil {
		return nil, mapServiceError(err)
	}
	file, err := h.Catalog.Get(ctx, access.OwnerID, fileID)
	if err != nil {
		return nil, mapServiceError(err)
	}
	sessionID := uuid.NewString()
	accountID := h.streamAccount(ctx, access.OwnerID, sessionID)
	token, expiresAt, err := h.StreamTokens.Sign(streamtoken.Claims{
		UserID: access.OwnerID, FileID: fileID, AccountID: accountID, SessionID: sessionID,
	})
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h.Downloader != nil && accountID > 0 {
		// Prime the selected account's file route while the player is launching.
		go func() {
			warmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := h.Downloader.WarmStream(warmCtx, access.OwnerID, fileID, accountID); err != nil {
				slog.WarnContext(warmCtx, "api.stream_warmup_failed",
					"file_id", fileID,
					"account_id", accountID,
					"error", err,
				)
			}
		}()
	}
	response := &gen.StreamToken{
		Path:      streamPath(file.Name, fileID, token),
		Token:     token,
		ExpiresAt: expiresAt,
		SessionId: sessionID,
	}
	if accountID > 0 {
		response.AccountId = gen.NewOptInt64(accountID)
	}
	return response, nil
}

func (h *Handler) streamAccount(ctx context.Context, userID int64, sessionID string) int64 {
	if h == nil || h.DownloadAccounts == nil || userID <= 0 {
		return 0
	}
	accounts, err := h.DownloadAccounts.DownloadAccounts(ctx, userID)
	if err != nil || len(accounts) == 0 {
		return 0
	}
	if len(accounts) == 1 {
		return accounts[0]
	}
	sum := sha256.Sum256([]byte(sessionID))
	index := binary.BigEndian.Uint64(sum[:8]) % uint64(len(accounts))
	return accounts[index]
}

func streamPath(name string, fileID uuid.UUID, token string) string {
	escaped := url.PathEscape(strings.TrimSpace(name))
	if escaped == "" || escaped == "." {
		escaped = "file"
	}
	return "/api/v1/files/" + fileID.String() + "/content/" + escaped + "?stream_token=" + url.QueryEscape(token)
}

// streamClaimsMatch rejects a signed token that was issued for another file.
func streamClaimsMatch(ctx context.Context, fileID uuid.UUID) bool {
	claims, ok := streamtoken.ClaimsFromContext(ctx)
	if !ok {
		return true
	}
	return claims.FileID == fileID
}

func streamPreferredAccount(ctx context.Context) int64 {
	claims, ok := streamtoken.ClaimsFromContext(ctx)
	if !ok {
		return 0
	}
	return claims.AccountID
}

func formatInt64(value int64) string { return strconv.FormatInt(value, 10) }
