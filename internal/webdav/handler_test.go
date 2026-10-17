package webdav

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/channels"
	"github.com/tgdrive/teldrive/v2/internal/principal"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

type countingAuthenticator struct {
	apiKeyCalls, bearerCalls int
}

func (a *countingAuthenticator) AuthenticateAPIKey(_ context.Context, key string) (principal.Identity, error) {
	a.apiKeyCalls++
	if key == "good-key" {
		return principal.Identity{UserID: 7, Source: "api_key"}, nil
	}
	return principal.Identity{}, errors.New("invalid API key")
}

func (a *countingAuthenticator) AuthenticateBearer(_ context.Context, token string) (principal.Identity, error) {
	a.bearerCalls++
	if token == "access-token" {
		return principal.Identity{UserID: 9, Source: "bearer"}, nil
	}
	return principal.Identity{}, errors.New("invalid token")
}

func testHandler(auth Authenticator, now *time.Time) *Handler {
	return &Handler{
		cfg: Config{Prefix: "/webdav"}, svc: Services{Auth: auth}, now: func() time.Time { return *now },
		authCache: make(map[[sha256.Size]byte]authEntry),
	}
}

func TestAuthenticateCachesVerifiedCredentials(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	auth := &countingAuthenticator{}
	h := testHandler(auth, &now)
	request := httptest.NewRequest("PROPFIND", "/webdav/", nil)
	request.SetBasicAuth("ignored", "good-key")

	for range 5 {
		identity, ok := h.authenticate(request)
		if !ok || identity.UserID != 7 {
			t.Fatalf("authenticate() = %+v, %v", identity, ok)
		}
	}
	if auth.apiKeyCalls != 1 {
		t.Fatalf("API key lookups = %d, want 1 within the cache lifetime", auth.apiKeyCalls)
	}
	now = now.Add(authCacheTTL + time.Second)
	if _, ok := h.authenticate(request); !ok || auth.apiKeyCalls != 2 {
		t.Fatalf("expired cache entry: ok=%v lookups=%d, want a second lookup", ok, auth.apiKeyCalls)
	}

	wrong := httptest.NewRequest("PROPFIND", "/webdav/", nil)
	wrong.SetBasicAuth("ignored", "bad-key")
	for range 2 {
		if _, ok := h.authenticate(wrong); ok {
			t.Fatal("bad key was accepted")
		}
	}
	if auth.bearerCalls != 0 {
		t.Fatal("Basic credentials must not be tried as access tokens")
	}
	if _, ok := h.authenticate(httptest.NewRequest("PROPFIND", "/webdav/", nil)); ok {
		t.Fatal("request without credentials was accepted")
	}
}

func TestAuthenticateAcceptsBearerAccessTokens(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	auth := &countingAuthenticator{}
	h := testHandler(auth, &now)
	request := httptest.NewRequest(http.MethodGet, "/webdav/", nil)
	request.Header.Set("Authorization", "Bearer access-token")
	identity, ok := h.authenticate(request)
	if !ok || identity.UserID != 9 || auth.apiKeyCalls != 1 || auth.bearerCalls != 1 {
		t.Fatalf("authenticate() = %+v, %v after %d/%d lookups", identity, ok, auth.apiKeyCalls, auth.bearerCalls)
	}
}

func TestAuthCacheStaysBounded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	h := testHandler(&acceptAll{}, &now)
	for i := range maxAuthCacheEntries + 10 {
		request := httptest.NewRequest(http.MethodGet, "/webdav/", nil)
		request.SetBasicAuth("u", fmt.Sprintf("key-%d", i))
		if _, ok := h.authenticate(request); !ok {
			t.Fatal("authenticate() failed")
		}
	}
	if len(h.authCache) > maxAuthCacheEntries {
		t.Fatalf("auth cache holds %d entries, want at most %d", len(h.authCache), maxAuthCacheEntries)
	}
}

type acceptAll struct{}

func (acceptAll) AuthenticateAPIKey(context.Context, string) (principal.Identity, error) {
	return principal.Identity{UserID: 1}, nil
}

func (acceptAll) AuthenticateBearer(context.Context, string) (principal.Identity, error) {
	return principal.Identity{UserID: 1}, nil
}

func TestLimitToRange(t *testing.T) {
	tests := map[string]int64{
		"":                    0,
		"bytes=0-99":          100,
		"bytes=100-199":       200,
		"bytes=100-":          0,
		"bytes=-500":          0,
		"bytes=0-5000":        1000,
		"bytes=0-1,5-9":       0,
		"items=0-1":           0,
		"bytes=abc-def":       0,
		"bytes= 10 - 19 ":     20,
		"bytes=999999-999999": 1000,
	}
	for header, want := range tests {
		reader := &lazyReader{size: 1000}
		reader.limitToRange(header)
		if reader.end != want {
			t.Fatalf("limitToRange(%q) end = %d, want %d", header, reader.end, want)
		}
	}
}

func TestResourcePath(t *testing.T) {
	h := &Handler{cfg: Config{Prefix: "/webdav"}}
	tests := map[string]string{
		"/webdav":               "/",
		"/webdav/":              "/",
		"/webdav/Movies":        "/Movies",
		"/webdav/Movies/":       "/Movies",
		"/webdav/a/../b":        "/b",
		"/webdav/../etc/passwd": "/etc/passwd",
	}
	for input, want := range tests {
		got, ok := h.resourcePath(input)
		if !ok || got != want {
			t.Fatalf("resourcePath(%q) = %q, %v; want %q", input, got, ok, want)
		}
	}
	for _, input := range []string{"/webdavx", "/other", "/"} {
		if _, ok := h.resourcePath(input); ok {
			t.Fatalf("resourcePath(%q) accepted a path outside the prefix", input)
		}
	}
}

func TestErrorStatus(t *testing.T) {
	tests := map[error]int{
		os.ErrNotExist:                         http.StatusNotFound,
		notExist("stat", "/x"):                 http.StatusNotFound,
		catalog.ErrNotFound:                    http.StatusNotFound,
		os.ErrPermission:                       http.StatusForbidden,
		catalog.ErrCycle:                       http.StatusForbidden,
		catalog.ErrConflict:                    http.StatusConflict,
		uploads.ErrNameConflict:                http.StatusConflict,
		catalog.ErrInvalidName:                 http.StatusBadRequest,
		channels.ErrChannelFull:                http.StatusInsufficientStorage,
		fmt.Errorf("wrapped: %w", os.ErrExist): http.StatusConflict,
		errors.New("database is down"):         http.StatusInternalServerError,
	}
	for err, want := range tests {
		if got := errorStatus(err); got != want {
			t.Fatalf("errorStatus(%v) = %d, want %d", err, got, want)
		}
	}
}

func TestNewValidatesConfiguration(t *testing.T) {
	for _, prefix := range []string{"", "/", "dav", "/dav/"} {
		if _, err := New(Config{Prefix: prefix, UploadPartSize: 1}, Services{}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(prefix %q) error = %v", prefix, err)
		}
	}
	if _, err := New(Config{Prefix: "/webdav", UploadPartSize: 1}, Services{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New() without services error = %v", err)
	}
}

func TestUnauthenticatedRequestsGetBasicChallenge(t *testing.T) {
	now := time.Now()
	h := testHandler(&countingAuthenticator{}, &now)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest("PROPFIND", "/webdav/", nil))
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") != basicRealm {
		t.Fatalf("status = %d, challenge = %q", recorder.Code, recorder.Header().Get("WWW-Authenticate"))
	}
}
