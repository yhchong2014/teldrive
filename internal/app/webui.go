package app

import (
	"bytes"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/tgdrive/teldrive/v2/internal/webdav"
	embeddedui "github.com/tgdrive/teldrive/v2/ui"
)

func newWebUIHandler() (http.Handler, error) {
	files, err := fs.Sub(embeddedui.StaticFS, "dist")
	if err != nil {
		return nil, fmt.Errorf("open embedded UI: %w", err)
	}
	if _, err := fs.Stat(files, "index.html"); err != nil {
		return nil, fmt.Errorf("inspect embedded UI index: %w", err)
	}
	return webUIHandler{files: files}, nil
}

type webUIHandler struct {
	files fs.FS
}

func (h webUIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self' blob:; script-src 'self'; style-src 'self' 'unsafe-inline' blob:; img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self' data: blob:; connect-src 'self' data: blob:; worker-src 'self' blob:; frame-src blob: data:; object-src 'none'; base-uri 'self'; form-action 'self'")
	requested := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if requested == "." || requested == "" {
		requested = "index.html"
	}
	if strings.HasPrefix(requested, ".") || strings.Contains(requested, "/.") {
		http.NotFound(w, r)
		return
	}
	content, stat, err := readWebUIFile(h.files, requested)
	if err != nil {
		if path.Ext(requested) != "" {
			http.NotFound(w, r)
			return
		}
		content, stat, err = readWebUIFile(h.files, "index.html")
	}
	if err != nil {
		http.Error(w, "UI is unavailable", http.StatusServiceUnavailable)
		return
	}
	if contentType := mime.TypeByExtension(path.Ext(stat.Name())); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if stat.Name() == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else if strings.Contains(stat.Name(), "-") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), content)
}

func readWebUIFile(files fs.FS, name string) (*bytes.Reader, fs.FileInfo, error) {
	file, err := files.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if stat.IsDir() {
		return nil, nil, fs.ErrNotExist
	}
	content, err := fs.ReadFile(files, name)
	if err != nil {
		return nil, nil, err
	}
	return bytes.NewReader(content), stat, nil
}

var webDAVMethodsOnce sync.Once

// registerWebDAVMethods teaches chi the WebDAV methods. chi answers unknown
// methods with 405 before routing, and routes only accept methods registered
// before they were added, so this runs before the router is built.
func registerWebDAVMethods() {
	webDAVMethodsOnce.Do(func() {
		for _, method := range webdav.Methods {
			chi.RegisterMethod(method)
		}
	})
}

// routeWebDAV mounts the WebDAV endpoint at prefix and below it.
func routeWebDAV(router chi.Router, prefix string, handler http.Handler) {
	router.Handle(prefix, handler)
	router.Handle(prefix+"/*", handler)
}

func routeApplication(router chi.Router, apiServer http.Handler, ui http.Handler) {
	router.Handle("/api/*", http.StripPrefix("/api", apiServer))
	router.Handle("/v1/*", apiServer)
	router.Handle("/health/*", apiServer)
	if ui != nil {
		router.Handle("/*", ui)
		return
	}
	router.Handle("/*", apiServer)
}

var _ http.Handler = webUIHandler{}
