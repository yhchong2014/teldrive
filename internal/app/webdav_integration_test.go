//go:build integration

package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	appapi "github.com/tgdrive/teldrive/v2/internal/api"
	"github.com/tgdrive/teldrive/v2/internal/app"
	"github.com/tgdrive/teldrive/v2/internal/config"
	testpostgres "github.com/tgdrive/teldrive/v2/internal/testutil/postgres"
)

const (
	davUser   int64 = 1001
	davAPIKey       = "dav-api-key"
)

type davAuthenticator struct{}

func (davAuthenticator) AuthenticateBearer(context.Context, string) (appapi.Identity, error) {
	return appapi.Identity{}, errors.New("invalid token")
}

func (davAuthenticator) AuthenticateAPIKey(_ context.Context, key string) (appapi.Identity, error) {
	if key != davAPIKey {
		return appapi.Identity{}, errors.New("invalid API key")
	}
	return appapi.Identity{UserID: davUser, Source: "api_key"}, nil
}

type davClient struct {
	t    *testing.T
	base string
	http *http.Client
}

func startWebDAVApp(t *testing.T, configure func(*config.Config)) *davClient {
	t.Helper()
	db := testpostgres.New(t)
	if _, err := db.Pool.Exec(context.Background(), "INSERT INTO users (user_id) VALUES ($1)", davUser); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Database.URL = db.URL
	cfg.HTTP.Address = "127.0.0.1:0"
	cfg.Telegram.Backend = "filesystem"
	cfg.Telegram.LocalRoot = t.TempDir()
	cfg.Telegram.AppID = 0
	cfg.Telegram.AppHash = ""
	cfg.Jobs.RunWorkers = false
	cfg.Security.SigningKey = "0123456789abcdef0123456789abcdef"
	cfg.Security.DataKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	// Small parts exercise multi-part uploads.
	cfg.WebDAV.UploadPartSize = 16 << 20
	if configure != nil {
		configure(&cfg)
	}
	application, err := app.New(context.Background(), cfg, app.Dependencies{Authenticator: davAuthenticator{}, Version: "integration"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- application.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(15 * time.Second):
			t.Error("application did not stop")
		}
	})
	client := &davClient{t: t, base: "http://" + listener.Addr().String(), http: &http.Client{Timeout: 30 * time.Second}}
	for range 50 {
		if response, err := client.http.Get(client.base + "/health/live"); err == nil {
			response.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return client
}

func (c *davClient) do(method, path string, body io.Reader, headers map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	request, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	request.SetBasicAuth("anyone", davAPIKey)
	for key, value := range headers {
		if key == "Authorization" && value == "" {
			request.Header.Del("Authorization")
			continue
		}
		request.Header.Set(key, value)
	}
	if reader, ok := body.(*chunkedReader); ok {
		request.ContentLength = -1
		request.Body = io.NopCloser(reader)
	}
	response, err := c.http.Do(request)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return response, content
}

func (c *davClient) expect(method, path string, body io.Reader, headers map[string]string, status int) []byte {
	c.t.Helper()
	response, content := c.do(method, path, body, headers)
	if response.StatusCode != status {
		c.t.Fatalf("%s %s status = %d, want %d; body=%s", method, path, response.StatusCode, status, content)
	}
	return content
}

// chunkedReader hides the length of its content so the request is chunked.
type chunkedReader struct{ io.Reader }

func randomBytes(t *testing.T, size int) []byte {
	t.Helper()
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestWebDAVServesTheDrive(t *testing.T) {
	c := startWebDAVApp(t, nil)

	response, _ := c.do("PROPFIND", "/webdav/", nil, map[string]string{"Authorization": "", "Depth": "1"})
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(response.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("anonymous PROPFIND status = %d, WWW-Authenticate = %q", response.StatusCode, response.Header.Get("WWW-Authenticate"))
	}
	wrong, err := http.NewRequest("PROPFIND", c.base+"/webdav/", nil)
	if err != nil {
		t.Fatal(err)
	}
	wrong.SetBasicAuth("anyone", "wrong-key")
	if response, err := c.http.Do(wrong); err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key PROPFIND = %v, %v", response, err)
	}

	c.expect("MKCOL", "/webdav/Movies", nil, nil, http.StatusCreated)
	c.expect("MKCOL", "/webdav/Movies", nil, nil, http.StatusMethodNotAllowed)
	c.expect("MKCOL", "/webdav/Missing/Child", nil, nil, http.StatusConflict)

	// A body spanning two 16 MiB parts with a known length.
	clip := randomBytes(t, 17<<20)
	c.expect(http.MethodPut, "/webdav/Movies/clip.mp4", bytes.NewReader(clip), nil, http.StatusCreated)
	clip = randomBytes(t, 300_000)
	c.expect(http.MethodPut, "/webdav/Movies/clip.mp4", bytes.NewReader(clip), nil, http.StatusNoContent)
	// A chunked body of unknown length spanning two parts.
	chunked := randomBytes(t, 16<<20+12345)
	c.expect(http.MethodPut, "/webdav/Movies/chunked.bin", &chunkedReader{bytes.NewReader(chunked)}, nil, http.StatusCreated)
	c.expect(http.MethodPut, "/webdav/Movies/empty.txt", bytes.NewReader(nil), nil, http.StatusCreated)
	c.expect(http.MethodPut, "/webdav/Nowhere/file.txt", strings.NewReader("x"), nil, http.StatusConflict)

	listing := string(c.expect("PROPFIND", "/webdav/Movies/", nil, map[string]string{"Depth": "1"}, http.StatusMultiStatus))
	for _, want := range []string{"/webdav/Movies/clip.mp4", "/webdav/Movies/chunked.bin", "/webdav/Movies/empty.txt", "video/mp4", "<D:getcontentlength>300000</D:getcontentlength>"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("PROPFIND listing lacks %q:\n%s", want, listing)
		}
	}
	c.expect("PROPFIND", "/webdav/", nil, map[string]string{"Depth": "infinity"}, http.StatusForbidden)
	root := string(c.expect("PROPFIND", "/webdav", nil, nil, http.StatusMultiStatus))
	if !strings.Contains(root, "/webdav/Movies/") {
		t.Fatalf("root PROPFIND lacks Movies:\n%s", root)
	}

	if got := c.expect(http.MethodGet, "/webdav/Movies/clip.mp4", nil, nil, http.StatusOK); !bytes.Equal(got, clip) {
		t.Fatalf("GET clip returned %d bytes that differ", len(got))
	}
	if got := c.expect(http.MethodGet, "/webdav/Movies/chunked.bin", nil, nil, http.StatusOK); !bytes.Equal(got, chunked) {
		t.Fatalf("GET chunked returned %d bytes that differ", len(got))
	}
	response, ranged := c.do(http.MethodGet, "/webdav/Movies/chunked.bin", nil, map[string]string{"Range": "bytes=16777000-16777999"})
	if response.StatusCode != http.StatusPartialContent || !bytes.Equal(ranged, chunked[16777000:16778000]) {
		t.Fatalf("ranged GET across parts = %d with %d bytes", response.StatusCode, len(ranged))
	}
	if contentRange := response.Header.Get("Content-Range"); contentRange != "bytes 16777000-16777999/16789561" {
		t.Fatalf("Content-Range = %q", contentRange)
	}
	response, _ = c.do(http.MethodHead, "/webdav/Movies/clip.mp4", nil, nil)
	if response.StatusCode != http.StatusOK || response.ContentLength != int64(len(clip)) || response.Header.Get("ETag") == "" {
		t.Fatalf("HEAD = %d, length %d, ETag %q", response.StatusCode, response.ContentLength, response.Header.Get("ETag"))
	}
	etag := response.Header.Get("ETag")
	c.expect(http.MethodGet, "/webdav/Movies/clip.mp4", nil, map[string]string{"If-None-Match": etag}, http.StatusNotModified)
	if page := string(c.expect(http.MethodGet, "/webdav/Movies/", nil, nil, http.StatusOK)); !strings.Contains(page, `href="/webdav/Movies/clip.mp4"`) {
		t.Fatalf("HTML listing lacks clip.mp4:\n%s", page)
	}

	c.expect("MOVE", "/webdav/Movies/clip.mp4", nil, map[string]string{"Destination": "http://elsewhere.example/webdav/Movies/renamed.mp4"}, http.StatusCreated)
	c.expect(http.MethodGet, "/webdav/Movies/clip.mp4", nil, nil, http.StatusNotFound)
	c.expect("MKCOL", "/webdav/Archive", nil, nil, http.StatusCreated)
	c.expect("MOVE", "/webdav/Movies/renamed.mp4", nil, map[string]string{"Destination": "/webdav/Archive/moved%20clip.mp4"}, http.StatusCreated)
	if got := c.expect(http.MethodGet, "/webdav/Archive/moved%20clip.mp4", nil, nil, http.StatusOK); !bytes.Equal(got, clip) {
		t.Fatal("moved file content differs")
	}
	c.expect("COPY", "/webdav/Archive/moved%20clip.mp4", nil, map[string]string{"Destination": "/webdav/Movies/copy.mp4"}, http.StatusCreated)
	if got := c.expect(http.MethodGet, "/webdav/Movies/copy.mp4", nil, nil, http.StatusOK); !bytes.Equal(got, clip) {
		t.Fatal("copied file content differs")
	}
	c.expect("COPY", "/webdav/Archive/moved%20clip.mp4", nil, map[string]string{"Destination": "/webdav/Movies/copy.mp4", "Overwrite": "F"}, http.StatusPreconditionFailed)
	c.expect("COPY", "/webdav/Movies/empty.txt", nil, map[string]string{"Destination": "/webdav/Movies/copy.mp4"}, http.StatusNoContent)
	if got := c.expect(http.MethodGet, "/webdav/Movies/copy.mp4", nil, nil, http.StatusOK); len(got) != 0 {
		t.Fatalf("overwritten copy has %d bytes", len(got))
	}
	c.expect("MOVE", "/webdav/Archive", nil, map[string]string{"Destination": "/webdav/Archive/inside"}, http.StatusForbidden)
	c.expect("MOVE", "/webdav/Archive/moved%20clip.mp4", nil, map[string]string{"Destination": "/other/place"}, http.StatusBadGateway)

	c.expect(http.MethodDelete, "/webdav/Movies/copy.mp4", nil, nil, http.StatusNoContent)
	c.expect(http.MethodGet, "/webdav/Movies/copy.mp4", nil, nil, http.StatusNotFound)
	c.expect(http.MethodDelete, "/webdav/Movies/copy.mp4", nil, nil, http.StatusNotFound)

	lockBody := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	response, _ = c.do("LOCK", "/webdav/Movies/locked.txt", strings.NewReader(lockBody), map[string]string{"Timeout": "Second-60"})
	token := response.Header.Get("Lock-Token")
	if response.StatusCode != http.StatusCreated || token == "" {
		t.Fatalf("LOCK = %d, token %q", response.StatusCode, token)
	}
	c.expect(http.MethodHead, "/webdav/Movies/locked.txt", nil, nil, http.StatusOK)
	c.expect("UNLOCK", "/webdav/Movies/locked.txt", nil, map[string]string{"Lock-Token": token}, http.StatusNoContent)

	patch := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:schemas-microsoft-com:"><D:set><D:prop><Z:Win32LastModifiedTime>Wed, 01 Jan 2025 00:00:00 GMT</Z:Win32LastModifiedTime></D:prop></D:set></D:propertyupdate>`
	if result := string(c.expect("PROPPATCH", "/webdav/Movies/chunked.bin", strings.NewReader(patch), nil, http.StatusMultiStatus)); !strings.Contains(result, "200 OK") {
		t.Fatalf("PROPPATCH result:\n%s", result)
	}
	c.expect("OPTIONS", "/webdav/", nil, nil, http.StatusOK)
	c.expect(http.MethodPost, "/webdav/Movies/chunked.bin", nil, nil, http.StatusMethodNotAllowed)
}

func TestWebDAVReadOnlyRejectsChanges(t *testing.T) {
	c := startWebDAVApp(t, func(cfg *config.Config) { cfg.WebDAV.ReadOnly = true })
	c.expect("PROPFIND", "/webdav/", nil, map[string]string{"Depth": "1"}, http.StatusMultiStatus)
	c.expect("MKCOL", "/webdav/Movies", nil, nil, http.StatusForbidden)
	c.expect(http.MethodPut, "/webdav/file.txt", strings.NewReader("x"), nil, http.StatusForbidden)
	c.expect("LOCK", "/webdav/file.txt", nil, nil, http.StatusForbidden)
}

func TestWebDAVCanBeDisabled(t *testing.T) {
	c := startWebDAVApp(t, func(cfg *config.Config) { cfg.WebDAV.Enabled = false })
	response, _ := c.do("PROPFIND", "/webdav/", nil, map[string]string{"Depth": "1"})
	if response.StatusCode == http.StatusMultiStatus {
		t.Fatal("PROPFIND succeeded with WebDAV disabled")
	}
}
