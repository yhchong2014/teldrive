// Package streamtoken issues and verifies short-lived signed URLs for media
// players such as VLC and PotPlayer. Those clients cannot set an Authorization
// header, so the credential travels in the query string. Unlike the opaque
// session hash v1 exposed, every token is scoped to one file, optionally to one
// Telegram account, and expires.
package streamtoken

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidToken = errors.New("invalid stream token")
	ErrExpiredToken = errors.New("stream token expired")
)

const version = "v1"

// Claims is the signed payload carried by a stream token.
type Claims struct {
	UserID    int64     `json:"u"`
	FileID    uuid.UUID `json:"f"`
	AccountID int64     `json:"a,omitempty"`
	SessionID string    `json:"s,omitempty"`
	IssuedAt  time.Time `json:"iat"`
	ExpiresAt time.Time `json:"exp"`
}

type Signer struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// NewSigner derives a dedicated HMAC key from the application signing key so a
// leaked stream token cannot be used to forge a JWT (or vice versa).
func NewSigner(signingKey string, ttl time.Duration) (*Signer, error) {
	if strings.TrimSpace(signingKey) == "" {
		return nil, errors.New("stream token signer requires a signing key")
	}
	if ttl <= 0 {
		return nil, errors.New("stream token lifetime must be positive")
	}
	sum := sha256.Sum256([]byte("teldrive/stream-token/" + signingKey))
	return &Signer{key: sum[:], ttl: ttl, now: time.Now}, nil
}

func (s *Signer) TTL() time.Duration {
	if s == nil {
		return 0
	}
	return s.ttl
}

// Sign returns the URL-safe token and its expiry.
func (s *Signer) Sign(claims Claims) (string, time.Time, error) {
	if s == nil {
		return "", time.Time{}, errors.New("stream token signer is not configured")
	}
	if claims.UserID <= 0 || claims.FileID == uuid.Nil {
		return "", time.Time{}, fmt.Errorf("%w: missing subject", ErrInvalidToken)
	}
	now := s.now().UTC()
	claims.IssuedAt = now
	claims.ExpiresAt = now.Add(s.ttl)
	if claims.SessionID == "" {
		claims.SessionID = uuid.NewString()
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("encode stream token: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return version + "." + encoded + "." + s.mac(encoded), claims.ExpiresAt, nil
}

// Verify decodes and authenticates a token.
func (s *Signer) Verify(token string) (Claims, error) {
	var claims Claims
	if s == nil {
		return claims, fmt.Errorf("%w: signer is not configured", ErrInvalidToken)
	}
	token = strings.TrimSpace(token)
	prefix, rest, ok := strings.Cut(token, ".")
	if !ok || prefix != version {
		return claims, ErrInvalidToken
	}
	encoded, signature, ok := strings.Cut(rest, ".")
	if !ok || encoded == "" || signature == "" {
		return claims, ErrInvalidToken
	}
	if !hmac.Equal([]byte(signature), []byte(s.mac(encoded))) {
		return claims, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return claims, ErrInvalidToken
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, ErrInvalidToken
	}
	if claims.UserID <= 0 || claims.FileID == uuid.Nil {
		return claims, ErrInvalidToken
	}
	if !claims.ExpiresAt.After(s.now()) {
		return claims, ErrExpiredToken
	}
	return claims, nil
}

func (s *Signer) mac(encoded string) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(version + "." + encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type contextKey struct{}

// WithClaims stores verified claims in a request context.
func WithClaims(ctx context.Context, claims Claims) context.Context {
	return context.WithValue(ctx, contextKey{}, claims)
}

// ClaimsFromContext returns claims attached by the API security handler.
func ClaimsFromContext(ctx context.Context) (Claims, bool) {
	claims, ok := ctx.Value(contextKey{}).(Claims)
	return claims, ok
}
