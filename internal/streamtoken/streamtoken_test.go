package streamtoken

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSignerRoundTrip(t *testing.T) {
	t.Parallel()
	signer, err := NewSigner("test-signing-key", time.Hour)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	fileID := uuid.New()
	token, expiresAt, err := signer.Sign(Claims{UserID: 42, FileID: fileID, AccountID: 7})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !strings.HasPrefix(token, version+".") {
		t.Fatalf("token %q is missing version prefix", token)
	}
	if time.Until(expiresAt) < 55*time.Minute {
		t.Fatalf("unexpected expiry %v", expiresAt)
	}
	claims, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.UserID != 42 || claims.FileID != fileID || claims.AccountID != 7 || claims.SessionID == "" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

func TestVerifyRejectsTamperedAndExpiredTokens(t *testing.T) {
	t.Parallel()
	signer, err := NewSigner("test-signing-key", time.Hour)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	fileID := uuid.New()
	token, _, err := signer.Sign(Claims{UserID: 1, FileID: fileID})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	tampered := token[:len(token)-2] + "aa"
	if _, err := signer.Verify(tampered); err == nil {
		t.Fatal("expected tampered token to fail verification")
	}
	other, err := NewSigner("another-signing-key", time.Hour)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	if _, err := other.Verify(token); err == nil {
		t.Fatal("expected a token signed with a different key to fail")
	}

	expiring, err := NewSigner("test-signing-key", time.Hour)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	expiring.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := expiring.Verify(token); err == nil {
		t.Fatal("expected an expired token to fail verification")
	}
}

func TestNewSignerValidatesInput(t *testing.T) {
	t.Parallel()
	if _, err := NewSigner("", time.Hour); err == nil {
		t.Fatal("expected empty signing key to be rejected")
	}
	if _, err := NewSigner("key", 0); err == nil {
		t.Fatal("expected zero TTL to be rejected")
	}
}
