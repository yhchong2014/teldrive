// Package webdav serves the TelDrive catalog over WebDAV (RFC 4918) so that
// file managers, media players and sync tools can browse, stream and upload
// files directly.
//
// Clients authenticate with HTTP Basic authentication using any user name and
// a TelDrive API key as the password, or with a bearer API key or access
// token. Reads stream through the regular download pipeline, so range requests
// fetch only the requested bytes from Telegram; uploads go through the regular
// upload sessions.
package webdav

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/channels"
	"github.com/tgdrive/teldrive/v2/internal/fileops"
	"github.com/tgdrive/teldrive/v2/internal/principal"
	"github.com/tgdrive/teldrive/v2/internal/transfer"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

const (
	// authCacheTTL bounds how long a verified credential is reused without
	// asking the database again. WebDAV clients send many small requests;
	// a revoked API key stops working within this time.
	authCacheTTL        = 30 * time.Second
	maxAuthCacheEntries = 1024
	basicRealm          = `Basic realm="TelDrive WebDAV", charset="UTF-8"`
)

var ErrInvalidConfig = errors.New("invalid WebDAV configuration")

// Methods lists the HTTP methods WebDAV adds; routers must accept them.
var Methods = []string{"PROPFIND", "PROPPATCH", "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK"}

type Config struct {
	// Prefix is the URL path the endpoint is mounted at, such as /webdav.
	Prefix   string
	ReadOnly bool
	// UploadPartSize is the Telegram part size of uploaded files.
	UploadPartSize int64
	// EncryptUploads encrypts uploads with ActiveKeyVersion.
	EncryptUploads   bool
	ActiveKeyVersion int32
}

// Authenticator verifies API keys and access tokens.
type Authenticator interface {
	AuthenticateBearer(context.Context, string) (principal.Identity, error)
	AuthenticateAPIKey(context.Context, string) (principal.Identity, error)
}

type Services struct {
	Auth       Authenticator
	Catalog    *catalog.Service
	Uploads    *uploads.Service
	Pipeline   *transfer.Pipeline
	Downloader *transfer.Downloader
	FileOps    *fileops.Service
}

type Handler struct {
	cfg Config
	svc Services
	fs  *fileSystem
	now func() time.Time

	mu   sync.Mutex
	davs map[int64]*webdav.Handler

	authMu    sync.Mutex
	authCache map[[sha256.Size]byte]authEntry
}

type authEntry struct {
	identity principal.Identity
	expires  time.Time
}

func New(cfg Config, svc Services) (*Handler, error) {
	cfg.Prefix = strings.TrimSpace(cfg.Prefix)
	if !strings.HasPrefix(cfg.Prefix, "/") || cfg.Prefix == "/" || cfg.Prefix != path.Clean(cfg.Prefix) {
		return nil, fmt.Errorf("%w: prefix %q", ErrInvalidConfig, cfg.Prefix)
	}
	if svc.Auth == nil || svc.Catalog == nil || svc.Uploads == nil || svc.Pipeline == nil || svc.Downloader == nil || svc.FileOps == nil {
		return nil, fmt.Errorf("%w: missing services", ErrInvalidConfig)
	}
	if cfg.UploadPartSize <= 0 {
		return nil, fmt.Errorf("%w: upload part size", ErrInvalidConfig)
	}
	if cfg.EncryptUploads && cfg.ActiveKeyVersion <= 0 {
		return nil, fmt.Errorf("%w: encrypted uploads need an active key version", ErrInvalidConfig)
	}
	h := &Handler{
		cfg: cfg, svc: svc, now: time.Now,
		davs: make(map[int64]*webdav.Handler), authCache: make(map[[sha256.Size]byte]authEntry),
	}
	h.fs = &fileSystem{handler: h, started: time.Now().UTC()}
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	identity, ok := h.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", basicRealm)
		http.Error(w, "Sign in with any user name and a TelDrive API key as the password.", http.StatusUnauthorized)
		return
	}
	ctx := principal.WithIdentity(r.Context(), identity)
	r = r.WithContext(withRequestState(ctx, identity.UserID))
	if h.cfg.ReadOnly && !isReadMethod(r.Method) {
		http.Error(w, "TelDrive WebDAV is read-only.", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.serveGet(w, r)
	case http.MethodPost:
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, COPY, MOVE, PROPFIND, PROPPATCH, LOCK, UNLOCK")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	case http.MethodPut:
		h.servePut(w, r)
	case "COPY", "MOVE":
		h.serveCopyMove(w, r)
	case "PROPFIND":
		switch depth := strings.TrimSpace(r.Header.Get("Depth")); {
		case depth == "":
			// RFC 4918 treats a missing Depth as infinity, which would walk the
			// whole drive. Clients that omit it want a listing.
			r.Header.Set("Depth", "1")
		case strings.EqualFold(depth, "infinity"):
			writeFiniteDepthError(w)
			return
		}
		h.dav(identity.UserID).ServeHTTP(w, r)
	default:
		// OPTIONS, DELETE, MKCOL, PROPPATCH, LOCK and UNLOCK.
		h.dav(identity.UserID).ServeHTTP(w, r)
	}
}

func isReadMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, "PROPFIND":
		return true
	default:
		return false
	}
}

// dav returns the protocol handler of a user. Every user gets a lock system of
// their own: locks are keyed by path, and paths are per user.
func (h *Handler) dav(userID int64) *webdav.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	if handler := h.davs[userID]; handler != nil {
		return handler
	}
	handler := &webdav.Handler{
		Prefix: h.cfg.Prefix, FileSystem: h.fs, LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.DebugContext(r.Context(), "webdav.request_failed", "method", r.Method, "path", r.URL.Path, "error", err)
			}
		},
	}
	h.davs[userID] = handler
	return handler
}

// authenticate accepts Basic credentials whose password is an API key, and
// bearer API keys or access tokens.
func (h *Handler) authenticate(r *http.Request) (principal.Identity, bool) {
	var secret string
	bearer := false
	if _, password, ok := r.BasicAuth(); ok {
		secret = password
	} else if value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		secret, bearer = value, true
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return principal.Identity{}, false
	}
	key := sha256.Sum256([]byte(secret))
	now := h.now()
	h.authMu.Lock()
	cached, ok := h.authCache[key]
	h.authMu.Unlock()
	if ok && now.Before(cached.expires) {
		return cached.identity, true
	}
	identity, err := h.svc.Auth.AuthenticateAPIKey(r.Context(), secret)
	if err != nil && bearer {
		identity, err = h.svc.Auth.AuthenticateBearer(r.Context(), secret)
	}
	if err != nil || identity.UserID <= 0 {
		return principal.Identity{}, false
	}
	h.authMu.Lock()
	if len(h.authCache) >= maxAuthCacheEntries {
		for entryKey, entry := range h.authCache {
			if !now.Before(entry.expires) {
				delete(h.authCache, entryKey)
			}
		}
		if len(h.authCache) >= maxAuthCacheEntries {
			clear(h.authCache)
		}
	}
	h.authCache[key] = authEntry{identity: identity, expires: now.Add(authCacheTTL)}
	h.authMu.Unlock()
	return identity, true
}

// resourcePath strips the endpoint prefix from a request path.
func (h *Handler) resourcePath(urlPath string) (string, bool) {
	if urlPath == h.cfg.Prefix {
		return "/", true
	}
	rest, ok := strings.CutPrefix(urlPath, h.cfg.Prefix+"/")
	if !ok {
		return "", false
	}
	return cleanPath(rest), true
}

func writeFiniteDepthError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>` +
		`<D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>`))
}

// writeError answers with the status that matches err.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := errorStatus(err)
	if status >= http.StatusInternalServerError && r.Context().Err() == nil {
		slog.ErrorContext(r.Context(), "webdav.request_failed", "method", r.Method, "path", r.URL.Path, "error", err)
	}
	http.Error(w, http.StatusText(status), status)
}

func errorStatus(err error) int {
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, catalog.ErrNotFound), errors.Is(err, fileops.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, os.ErrPermission), errors.Is(err, catalog.ErrCycle):
		return http.StatusForbidden
	case errors.Is(err, os.ErrExist), errors.Is(err, catalog.ErrConflict), errors.Is(err, uploads.ErrNameConflict),
		errors.Is(err, catalog.ErrInvalidParent), errors.Is(err, uploads.ErrInvalidParent):
		return http.StatusConflict
	case errors.Is(err, catalog.ErrInvalidName), errors.Is(err, uploads.ErrInvalidInput), errors.Is(err, fileops.ErrInvalidInput),
		errors.Is(err, transfer.ErrBodyTooShort), errors.Is(err, transfer.ErrBodyTooLong):
		return http.StatusBadRequest
	case errors.Is(err, transfer.ErrRangeNotSatisfiable):
		return http.StatusRequestedRangeNotSatisfiable
	case errors.Is(err, channels.ErrNoSelected), errors.Is(err, channels.ErrChannelFull), errors.Is(err, channels.ErrAutoCreateOff):
		return http.StatusInsufficientStorage
	default:
		return http.StatusInternalServerError
	}
}
