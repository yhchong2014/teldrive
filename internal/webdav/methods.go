package webdav

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/fileops"
	"github.com/tgdrive/teldrive/v2/internal/transfer"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

// serveGet streams a file, honoring Range and conditional headers, or renders
// a folder as an HTML listing for browsers.
func (h *Handler) serveGet(w http.ResponseWriter, r *http.Request) {
	name, ok := h.resourcePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	resolved, err := h.fs.resolve(r.Context(), name)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if resolved.isDir() {
		h.serveDirectory(w, r, resolved)
		return
	}
	// Playback of a long video can keep one response open for hours; the
	// server-wide write timeout must not cut it.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	state := stateFrom(r.Context())
	reader := newLazyReader(r.Context(), h.svc.Downloader, state.userID, resolved)
	reader.limitToRange(r.Header.Get("Range"))
	defer reader.Close()
	w.Header().Set("ETag", resolved.etag())
	w.Header().Set("Content-Type", resolved.contentType())
	w.Header().Set("X-Accel-Buffering", "no")
	http.ServeContent(w, r, resolved.file.Name, resolved.modTime(h.fs.started), reader)
}

var listingTemplate = template.Must(template.New("listing").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>body{font:15px system-ui,sans-serif;margin:2rem}td{padding:.25rem 1.5rem .25rem 0}a{text-decoration:none}.n{text-align:right}</style>
</head><body><h1>{{.Title}}</h1><table>
{{if .Parent}}<tr><td><a href="{{.Parent}}">../</a></td><td></td><td></td></tr>{{end}}
{{range .Entries}}<tr><td><a href="{{.Href}}">{{.Name}}</a></td><td class="n">{{.Size}}</td><td>{{.Modified}}</td></tr>
{{end}}</table></body></html>
`))

type listingEntry struct {
	Name, Href, Size, Modified string
}

func (h *Handler) serveDirectory(w http.ResponseWriter, r *http.Request, dir *entry) {
	children, err := h.fs.list(r.Context(), dir)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	base := path.Join(h.cfg.Prefix, dir.path)
	data := struct {
		Title, Parent string
		Entries       []listingEntry
	}{Title: "TelDrive " + dir.path}
	if !dir.isRoot() {
		data.Parent = (&url.URL{Path: path.Dir(base) + "/"}).EscapedPath()
	}
	for _, child := range children {
		item := listingEntry{Name: child.file.Name, Modified: child.modTime(h.fs.started).UTC().Format("2006-01-02 15:04")}
		href := path.Join(base, child.file.Name)
		if child.isDir() {
			item.Name += "/"
			href += "/"
		} else {
			item.Size = formatSize(child.size())
		}
		item.Href = (&url.URL{Path: href}).EscapedPath()
		data.Entries = append(data.Entries, item)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	if r.Method == http.MethodHead {
		return
	}
	_ = listingTemplate.Execute(w, data)
}

func formatSize(size int64) string {
	const unit = 1024
	if size < unit {
		return strconv.FormatInt(size, 10) + " B"
	}
	value, suffix := float64(size), "KMGTPE"
	index := -1
	for value >= unit && index < len(suffix)-1 {
		value /= unit
		index++
	}
	return fmt.Sprintf("%.1f %ciB", value, suffix[index])
}

// lazyReader is an io.ReadSeeker over a catalog file. It opens a download at
// the current offset on the first Read after a Seek, so http.ServeContent can
// probe the size and HEAD requests never touch Telegram.
type lazyReader struct {
	ctx        context.Context
	downloader *transfer.Downloader
	userID     int64
	fileID     uuid.UUID
	size       int64
	// end bounds the first download to the requested range, so that a small
	// range request does not prefetch the rest of the file.
	end     int64
	pos     int64
	body    io.ReadCloser
	bodyPos int64
}

func newLazyReader(ctx context.Context, downloader *transfer.Downloader, userID int64, file *entry) *lazyReader {
	reader := &lazyReader{ctx: ctx, downloader: downloader, userID: userID, size: file.size()}
	if id := file.id(); id != nil {
		reader.fileID = *id
	}
	return reader
}

// limitToRange bounds the first download to a single requested byte range.
func (r *lazyReader) limitToRange(header string) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return
	}
	first, last, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return
	}
	if first == "" {
		// A suffix range always reaches the end of the file.
		return
	}
	if last == "" {
		return
	}
	end, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64)
	if err != nil || end < 0 {
		return
	}
	r.end = min(end+1, r.size)
}

func (r *lazyReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.body != nil && r.bodyPos != r.pos {
		_ = r.body.Close()
		r.body = nil
	}
	fresh := r.body == nil
	if fresh {
		length := r.size - r.pos
		if r.end > r.pos {
			length = r.end - r.pos
		}
		download, err := r.downloader.Open(r.ctx, transfer.DownloadRequest{
			UserID: r.userID, FileID: r.fileID, Offset: r.pos, Length: length,
		})
		if err != nil {
			return 0, err
		}
		r.body, r.bodyPos = download.Reader, r.pos
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	r.bodyPos += int64(n)
	if errors.Is(err, io.EOF) && r.pos < r.size {
		// The bounded download ended; the next Read opens the next one.
		_ = r.body.Close()
		r.body = nil
		if n == 0 && fresh {
			return 0, io.ErrUnexpectedEOF
		}
		if n == 0 {
			return r.Read(p)
		}
		return n, nil
	}
	return n, err
}

func (r *lazyReader) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.pos + offset
	case io.SeekEnd:
		next = r.size + offset
	default:
		return r.pos, os.ErrInvalid
	}
	if next < 0 {
		return r.pos, os.ErrInvalid
	}
	r.pos = next
	return next, nil
}

func (r *lazyReader) Close() error {
	if r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}

// servePut stores the request body as a file, replacing an existing file.
// Bodies with a known length stream straight to Telegram part by part; bodies
// of unknown length are buffered one part at a time in a temporary file.
func (h *Handler) servePut(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	state := stateFrom(ctx)
	name, ok := h.resourcePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	parent, base, err := h.fs.resolveParent(ctx, name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "The parent folder does not exist.", http.StatusConflict)
			return
		}
		h.writeError(w, r, err)
		return
	}
	existing, err := h.fs.resolve(ctx, name)
	switch {
	case err == nil && existing.isDir():
		http.Error(w, "A folder exists at this path.", http.StatusMethodNotAllowed)
		return
	case err != nil && !errors.Is(err, os.ErrNotExist):
		h.writeError(w, r, err)
		return
	}
	// Large uploads can take longer than the server-wide timeouts.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Time{})
	size := r.ContentLength
	if size < 0 {
		// macOS Finder streams chunked bodies and announces the length here.
		if value, parseErr := strconv.ParseInt(r.Header.Get("X-Expected-Entity-Length"), 10, 64); parseErr == nil && value >= 0 {
			size = value
		}
	}
	var mimeType *string
	if value := fileContentType(base, uploadContentType(r)); value != "application/octet-stream" {
		mimeType = &value
	}
	file, err := h.createFile(ctx, state.userID, parent, base, r.Body, size, mimeType, modTimeHeader(r), sqlcgen.NameConflictPolicyReplace)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	clean := cleanPath(name)
	state.forget(clean)
	created := &entry{path: clean, file: file}
	state.store(clean, created)
	w.Header().Set("ETag", created.etag())
	if existing != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// uploadContentType returns the request content type unless it is the
// generic default that clients send for everything.
func uploadContentType(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Content-Type"))
	if value == "" || strings.HasPrefix(value, "application/octet-stream") {
		return ""
	}
	return value
}

// modTimeHeader reads the modification time sync clients such as rclone and
// Nextcloud send in X-OC-Mtime as Unix seconds.
func modTimeHeader(r *http.Request) time.Time {
	seconds, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("X-OC-Mtime")), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

// createFile uploads body as name inside parent. size -1 means unknown; a nil
// body creates an empty file. A nil mimeType is derived from the name.
func (h *Handler) createFile(ctx context.Context, userID int64, parent *entry, name string, body io.Reader, size int64, mimeType *string, modTime time.Time, policy sqlcgen.NameConflictPolicy) (*sqlcgen.File, error) {
	if body == nil {
		body, size = strings.NewReader(""), 0
	}
	partSize := h.cfg.UploadPartSize
	input := uploads.CreateInput{
		UserID: userID, ParentID: parent.id(), Name: name, ExpectedSize: size, ModTime: modTime,
		ConflictPolicy: policy, PartSize: partSize, MIMEType: mimeType,
	}
	if input.MIMEType == nil {
		if value := fileContentType(name, ""); value != "application/octet-stream" {
			input.MIMEType = &value
		}
	}
	if h.cfg.EncryptUploads {
		version := h.cfg.ActiveKeyVersion
		input.Encryption, input.EncryptionKeyVersion = true, &version
	}

	var spool *partSpool
	if size < 0 {
		spool = &partSpool{}
		defer spool.Close()
		// A body that fits in one part has a known size after all.
		n, err := spool.fill(body, partSize)
		if err != nil {
			return nil, err
		}
		if n < partSize {
			input.ExpectedSize, size = n, n
			body = spool.reader()
			spool.pending = false
		}
	}
	session, err := h.svc.Uploads.Create(ctx, input)
	if err != nil {
		return nil, err
	}
	uploadID, ok := dbtypes.GoogleUUID(session.ID)
	if !ok {
		return nil, uploads.ErrInvalidInput
	}
	if err := h.uploadParts(ctx, userID, uploadID, body, size, partSize, spool); err != nil {
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = h.svc.Uploads.Abort(abortCtx, userID, uploadID)
		return nil, err
	}
	return h.svc.Uploads.Complete(ctx, userID, uploadID)
}

func (h *Handler) uploadParts(ctx context.Context, userID int64, uploadID uuid.UUID, body io.Reader, size, partSize int64, spool *partSpool) error {
	upload := func(partNo int32, reader io.Reader, plainSize int64) error {
		_, err := h.svc.Pipeline.UploadPart(ctx, transfer.UploadPartRequest{
			UserID: userID, UploadID: uploadID, PartNo: partNo, PlainSize: plainSize, Body: reader,
		})
		return err
	}
	if size >= 0 {
		var partNo int32 = 1
		for offset := int64(0); offset < size; offset += partSize {
			plainSize := min(partSize, size-offset)
			if err := upload(partNo, io.LimitReader(body, plainSize), plainSize); err != nil {
				return err
			}
			partNo++
		}
		return nil
	}
	// Unknown length: the spool holds the first full part already.
	for partNo := int32(1); ; partNo++ {
		if !spool.pending {
			n, err := spool.fill(body, partSize)
			if err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
		}
		spool.pending = false
		if err := upload(partNo, spool.reader(), spool.size); err != nil {
			return err
		}
		if spool.size < partSize {
			return nil
		}
	}
}

// partSpool buffers one upload part of a body with unknown length.
type partSpool struct {
	file    *os.File
	size    int64
	pending bool
}

func (s *partSpool) fill(body io.Reader, partSize int64) (int64, error) {
	if s.file == nil {
		file, err := os.CreateTemp("", "teldrive-webdav-*.part")
		if err != nil {
			return 0, fmt.Errorf("create upload spool: %w", err)
		}
		_ = os.Remove(file.Name())
		s.file = file
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	if err := s.file.Truncate(0); err != nil {
		return 0, err
	}
	n, err := io.CopyN(s.file, body, partSize)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("read upload body: %w", err)
	}
	s.size, s.pending = n, n > 0
	return n, nil
}

func (s *partSpool) reader() io.Reader {
	return io.NewSectionReader(s.file, 0, s.size)
}

func (s *partSpool) Close() {
	if s.file != nil {
		_ = s.file.Close()
	}
}

// serveCopyMove copies or moves within the drive. Copies are server-side
// Telegram copies, never downloads and uploads. The Destination host is not
// compared with the request host, so the endpoint works behind proxies that
// rewrite Host.
func (h *Handler) serveCopyMove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	state := stateFrom(ctx)
	sourcePath, ok := h.resourcePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	destination, err := url.Parse(r.Header.Get("Destination"))
	if err != nil || destination.Path == "" {
		http.Error(w, "Invalid Destination header.", http.StatusBadRequest)
		return
	}
	destinationPath, ok := h.resourcePath(destination.Path)
	if !ok {
		http.Error(w, "The destination is outside this WebDAV endpoint.", http.StatusBadGateway)
		return
	}
	if sourcePath == "/" || destinationPath == "/" || sourcePath == destinationPath ||
		strings.HasPrefix(destinationPath, sourcePath+"/") {
		http.Error(w, "The source and destination overlap.", http.StatusForbidden)
		return
	}
	source, err := h.fs.resolve(ctx, sourcePath)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	parent, base, err := h.fs.resolveParent(ctx, destinationPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "The destination folder does not exist.", http.StatusConflict)
			return
		}
		h.writeError(w, r, err)
		return
	}
	existing, err := h.fs.resolve(ctx, destinationPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		h.writeError(w, r, err)
		return
	}
	if existing != nil {
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Overwrite")), "F") {
			http.Error(w, "The destination exists.", http.StatusPreconditionFailed)
			return
		}
		// RFC 4918 9.9.3: delete the destination before the operation.
		if _, err := h.svc.Catalog.Trash(ctx, state.userID, *existing.id()); err != nil {
			h.writeError(w, r, err)
			return
		}
		state.forget(existing.path)
	}
	if r.Method == "MOVE" {
		err = h.move(ctx, source, parent, base)
	} else {
		err = h.copy(ctx, source, parent, base, strings.TrimSpace(r.Header.Get("Depth")) == "0")
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	state.forget(source.path)
	state.forget(cleanPath(destinationPath))
	if existing != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// move renames and reparents source to parent/name.
func (h *Handler) move(ctx context.Context, source *entry, parent *entry, name string) error {
	state := stateFrom(ctx)
	if state == nil || source.isRoot() {
		return os.ErrPermission
	}
	sourceID := *source.id()
	sourceParent, _ := dbtypes.GoogleUUID(source.file.ParentID)
	parentID := parent.id()
	sameParent := (parentID == nil && !source.file.ParentID.Valid) || (parentID != nil && source.file.ParentID.Valid && *parentID == sourceParent)
	rename := func() error {
		if source.file.Name == name {
			return nil
		}
		_, err := h.svc.Catalog.Rename(ctx, state.userID, sourceID, nil, name)
		return err
	}
	if sameParent {
		return rename()
	}
	_, err := h.svc.Catalog.MoveWithPolicy(ctx, state.userID, sourceID, parentID, nil, "fail")
	if errors.Is(err, catalog.ErrConflict) && source.file.Name != name {
		// The destination holds another entry with the current name: rename
		// first, then move.
		if err := rename(); err != nil {
			return err
		}
		_, err = h.svc.Catalog.MoveWithPolicy(ctx, state.userID, sourceID, parentID, nil, "fail")
		return err
	}
	if err != nil {
		return err
	}
	return rename()
}

// copy copies source to parent/name. A shallow copy of a folder creates an
// empty folder.
func (h *Handler) copy(ctx context.Context, source *entry, parent *entry, name string, shallow bool) error {
	state := stateFrom(ctx)
	if state == nil || source.isRoot() {
		return os.ErrPermission
	}
	if shallow && source.isDir() {
		_, err := h.svc.Catalog.CreateFolder(ctx, catalog.CreateFolderInput{UserID: state.userID, ParentID: parent.id(), Name: name})
		return err
	}
	_, err := h.svc.FileOps.Copy(ctx, fileops.CopyInput{
		UserID: state.userID, FileID: *source.id(), ParentID: parent.id(), Name: &name,
		ConflictPolicy: sqlcgen.NameConflictPolicyFail,
	})
	return err
}
