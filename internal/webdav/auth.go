// Package webdav serves a WebDAV view of a user's Teldrive catalog using the
// dedicated WebDAV username and password configured in Settings.
package webdav

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
)

var (
	ErrInvalidCredentials = errors.New("invalid WebDAV credentials")
	ErrCredentialConflict = errors.New("WebDAV username already exists")
)

const bcryptCost = 12

type cachedCredential struct {
	userID  int64
	expires time.Time
}

// Credentials stores and verifies per-user WebDAV logins. Password hashes use
// bcrypt; successful authentications are cached briefly so rclone's frequent
// requests do not pay the hash cost on every round trip.
type Credentials struct {
	queries *sqlcgen.Queries
	ttl     time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]cachedCredential
}

func NewCredentials(queries *sqlcgen.Queries, ttl time.Duration) (*Credentials, error) {
	if queries == nil {
		return nil, errors.New("webdav credentials require database queries")
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Credentials{queries: queries, ttl: ttl, now: time.Now, cache: make(map[string]cachedCredential)}, nil
}

func (c *Credentials) Set(ctx context.Context, userID int64, username, password string) (*sqlcgen.WebdavCredential, error) {
	username = strings.TrimSpace(username)
	if userID <= 0 || username == "" || len(password) < 8 {
		return nil, fmt.Errorf("%w: username and a password of at least 8 characters are required", ErrInvalidCredentials)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash WebDAV password: %w", err)
	}
	row, err := c.queries.UpsertWebDAVCredential(ctx, sqlcgen.UpsertWebDAVCredentialParams{
		UserID: userID, Username: username, PasswordHash: string(hash),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrCredentialConflict
		}
		return nil, fmt.Errorf("store WebDAV credential: %w", err)
	}
	c.invalidate()
	return row, nil
}

func (c *Credentials) Delete(ctx context.Context, userID int64) error {
	if err := c.queries.DeleteWebDAVCredential(ctx, userID); err != nil {
		return fmt.Errorf("delete WebDAV credential: %w", err)
	}
	c.invalidate()
	return nil
}

func (c *Credentials) GetByUser(ctx context.Context, userID int64) (*sqlcgen.WebdavCredential, error) {
	row, err := c.queries.GetWebDAVCredentialByUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load WebDAV credential: %w", err)
	}
	return row, nil
}

// Authenticate validates the Basic credentials and returns the owning user.
func (c *Credentials) Authenticate(ctx context.Context, username, password string) (int64, bool) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return 0, false
	}
	key := cacheKey(username, password)
	c.mu.Lock()
	entry, ok := c.cache[key]
	c.mu.Unlock()
	if ok && c.now().Before(entry.expires) {
		return entry.userID, true
	}
	row, err := c.queries.GetWebDAVCredentialByUsername(ctx, username)
	if err != nil {
		return 0, false
	}
	if bcrypt.CompareHashAndPassword([]byte(row.PasswordHash), []byte(password)) != nil {
		return 0, false
	}
	c.mu.Lock()
	c.cache[key] = cachedCredential{userID: row.UserID, expires: c.now().Add(c.ttl)}
	c.mu.Unlock()
	return row.UserID, true
}

func (c *Credentials) invalidate() {
	c.mu.Lock()
	c.cache = make(map[string]cachedCredential)
	c.mu.Unlock()
}

func cacheKey(username, password string) string {
	sum := sha256.Sum256([]byte(password))
	return username + "\x00" + hex.EncodeToString(sum[:])
}

func isUniqueViolation(err error) bool {
	type sqlState interface{ SQLState() string }
	var state sqlState
	if errors.As(err, &state) {
		return state.SQLState() == "23505"
	}
	return false
}
