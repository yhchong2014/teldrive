package webdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
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

var (
	ErrNotFound = errors.New("webdav path not found")
	ErrConflict = errors.New("webdav destination already exists")
	ErrInvalid  = errors.New("invalid webdav request")
)

const (
	maxPropfindEntries = 20000
	maxPropfindDepth   = 16
	putPartSize        = int64(512 * 1024 * 1024)
)

// Handler implements the WebDAV verbs Teldrive supports. It is mounted on the
// main HTTP listener below the configured prefix (default /dav).
type Handler struct {
	Catalog    *catalog.Service
	Files      *fileops.Service
	Uploads    *uploads.Service
	Pipeline   *transfer.Pipeline
	Downloader *transfer.Downloader
	Auth       *Credentials
	Realm      string
	Prefix     string
}

func New(catalogService *catalog.Service, files *fileops.Service, uploadServices *uploads.Service, pipeline *transfer.Pipeline, downloader *transfer.Downloader, auth *Credentials, realm, prefix string) (*Handler, error) {
	if catalogService == nil || files == nil || uploadServices == nil || pipeline == nil || downloader == nil || auth == nil {
		return nil, errors.New("webdav handler requires all services")
	}
	if strings.TrimSpace(realm) == "" {
		realm = "teldrive"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return &Handler{
		Catalog: catalogService, Files: files, Uploads: uploadServices, Pipeline: pipeline,
		Downloader: downloader, Auth: auth, Realm: realm, Prefix: strings.TrimSuffix(prefix, "/"),
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("DAV", "1, 2")
	w.Header().Set("MS-Author-Via", "DAV")
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", allowHeader)
		w.WriteHeader(http.StatusOK)
		return
	}
	userID, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	requestPath, err := normalizePath(r.URL.Path)
	if err != nil {
		writeStatus(w, http.StatusBadRequest)
		return
	}
	switch r.Method {
	case "PROPFIND":
		h.propfind(ctx, w, r, userID, requestPath)
	case http.MethodGet, http.MethodHead:
		h.get(ctx, w, r, userID, requestPath)
	case http.MethodPut:
		h.put(ctx, w, r, userID, requestPath)
	case "MKCOL":
		h.mkcol(ctx, w, r, userID, requestPath)
	case http.MethodDelete:
		h.delete(ctx, w, r, userID, requestPath)
	case "MOVE":
		h.move(ctx, w, r, userID, requestPath)
	case "COPY":
		h.copy(ctx, w, r, userID, requestPath)
	case "PROPPATCH":
		h.proppatch(ctx, w, r, userID, requestPath)
	case "LOCK":
		h.lock(ctx, w, r, userID, requestPath)
	case "UNLOCK":
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", allowHeader)
		writeStatus(w, http.StatusMethodNotAllowed)
	}
}

const allowHeader = "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, MKCOL, COPY, MOVE, LOCK, UNLOCK"

func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (int64, bool) {
	username, password, ok := r.BasicAuth()
	if ok {
		if userID, valid := h.Auth.Authenticate(r.Context(), username, password); valid {
			return userID, true
		}
	}
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm=%q, charset="UTF-8"`, h.Realm))
	writeStatus(w, http.StatusUnauthorized)
	return 0, false
}

// ---- verbs -------------------------------------------------------------

func (h *Handler) propfind(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64, requestPath string) {
	depth := strings.ToLower(strings.TrimSpace(r.Header.Get("Depth")))
	switch depth {
	case "", "infinity":
		depth = "infinity"
	case "0", "1":
	default:
		writeStatus(w, http.StatusBadRequest)
		return
	}
	file, err := h.resolve(ctx, userID, requestPath)
	if err != nil {
		writeWebDAVError(w, err)
		return
	}
	response := multiStatus{XMLNS: "DAV:"}
	add := func(item *sqlcgen.File, itemPath string) bool {
		response.Responses = append(response.Responses, propResponse{
			Href:     h.href(itemPath, item.Kind == sqlcgen.FileKindFolder),
			Propstat: []propstat{{Status: "HTTP/1.1 200 OK", Prop: h.fileProps(item)}},
		})
		return len(response.Responses) < maxPropfindEntries
	}
	if file == nil { // root collection
		response = addRootRoot(h, response)
	} else {
		if !add(file, requestPath) {
			writeStatus(w, http.StatusInsufficientStorage)
			return
		}
	}
	if depth != "0" && (file == nil || file.Kind == sqlcgen.FileKindFolder) {
		var rootID *uuid.UUID
		if file != nil {
			id, ok := dbtypes.GoogleUUID(file.ID)
			if !ok {
				writeWebDAVError(w, ErrNotFound)
				return
			}
			rootID = &id
		}
		if !h.walk(ctx, userID, rootID, path.Join(requestPath, "/"), depth == "infinity", 1, add) {
			writeStatus(w, http.StatusInsufficientStorage)
			return
		}
	}
	writeXML(w, http.StatusMultiStatus, response)
}

func addRootRoot(h *Handler, response multiStatus) multiStatus {
	root := prop{DisplayName: "root", ResourceType: &resourceType{Collection: &struct{}{}}}
	response.Responses = append(response.Responses, propResponse{
		Href:     h.href("/", true),
		Propstat: []propstat{{Status: "HTTP/1.1 200 OK", Prop: root}},
	})
	return response
}

// walk appends children depth-first. It returns false when the entry budget is
// exhausted so callers can fail the request instead of streaming forever.
func (h *Handler) walk(ctx context.Context, userID int64, parentID *uuid.UUID, parentHref string, recursive bool, depth int, add func(*sqlcgen.File, string) bool) bool {
	if depth > maxPropfindDepth {
		return true
	}
	children, err := h.listChildren(ctx, userID, parentID)
	if err != nil {
		slog.WarnContext(ctx, "webdav.list_children", "error", err)
		return true
	}
	for _, child := range children {
		href := path.Join(parentHref, child.Name)
		if !add(child, href) {
			return false
		}
		if recursive && child.Kind == sqlcgen.FileKindFolder {
			id, ok := dbtypes.GoogleUUID(child.ID)
			if !ok {
				continue
			}
			if !h.walk(ctx, userID, &id, href, recursive, depth+1, add) {
				return false
			}
		}
	}
	return true
}

func (h *Handler) get(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64, requestPath string) {
	file, err := h.resolve(ctx, userID, requestPath)
	if err != nil || file == nil || file.Kind != sqlcgen.FileKindFile {
		if err == ErrNotFound || file == nil {
			writeStatus(w, http.StatusNotFound)
			return
		}
		writeWebDAVError(w, err)
		return
	}
	fileID, ok := dbtypes.GoogleUUID(file.ID)
	if !ok || !file.Size.Valid {
		writeStatus(w, http.StatusInternalServerError)
		return
	}
	size := file.Size.Int64
	start, length, partial, satisfiable := parseRange(r.Header.Get("Range"), size)
	if !satisfiable {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		writeStatus(w, http.StatusRequestedRangeNotSatisfiable)
		return
	}
	etag := fileETag(file)
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		writeStatus(w, http.StatusNotModified)
		return
	}
	contentType := "application/octet-stream"
	if file.MimeType.Valid && file.MimeType.String != "" {
		contentType = file.MimeType.String
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Last-Modified", file.ModTime.Time.UTC().Format(http.TimeFormat))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if r.Method == http.MethodHead {
		writeStatus(w, http.StatusOK)
		return
	}
	if length == 0 {
		writeStatus(w, http.StatusOK)
		return
	}
	download, err := h.Downloader.Open(ctx, transfer.DownloadRequest{
		UserID: userID, FileID: fileID, Offset: start, Length: length,
	})
	if err != nil {
		writeWebDAVError(w, err)
		return
	}
	defer download.Reader.Close()
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	_, _ = io.CopyN(w, download.Reader, length)
}

func (h *Handler) put(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64, requestPath string) {
	if requestPath == "/" {
		writeStatus(w, http.StatusMethodNotAllowed)
		return
	}
	parentPath, name := splitPath(requestPath)
	parent, err := h.resolve(ctx, userID, parentPath)
	if err != nil || (parent != nil && parent.Kind != sqlcgen.FileKindFolder) {
		writeStatus(w, http.StatusConflict)
		return
	}
	var parentID *uuid.UUID
	if parent != nil {
		id, ok := dbtypes.GoogleUUID(parent.ID)
		if !ok {
			writeStatus(w, http.StatusInternalServerError)
			return
		}
		parentID = &id
	}
	existing, _ := h.resolve(ctx, userID, requestPath)
	policy := sqlcgen.NameConflictPolicyReplace
	if existing == nil {
		policy = sqlcgen.NameConflictPolicyFail
	}

	reader := io.Reader(r.Body)
	size := r.ContentLength
	var spool *os.File
	if size < 0 {
		// Chunked uploads have no length up front: spool to disk, then upload.
		tmp, err := os.CreateTemp("", "teldrive-webdav-*")
		if err != nil {
			writeStatus(w, http.StatusInternalServerError)
			return
		}
		defer func() {
			tmp.Close()
			os.Remove(tmp.Name())
		}()
		written, err := io.Copy(tmp, r.Body)
		if err != nil {
			writeStatus(w, http.StatusBadRequest)
			return
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			writeStatus(w, http.StatusInternalServerError)
			return
		}
		spool, reader, size = tmp, tmp, written
	}
	mimeType := detectMimeType(name, r.Header.Get("Content-Type"))
	session, err := h.Uploads.Create(ctx, uploads.CreateInput{
		UserID: userID, ParentID: parentID, Name: name, ExpectedSize: size,
		MIMEType: &mimeType, ModTime: time.Now().UTC(), ConflictPolicy: policy, PartSize: putPartSize,
	})
	if err != nil {
		writeWebDAVError(w, err)
		return
	}
	if err := h.storeUpload(ctx, userID, session, reader, size); err != nil {
		_, _ = h.Uploads.Abort(ctx, userID, uuid.UUID(session.ID.Bytes))
		slog.WarnContext(ctx, "webdav.put_failed", "path", requestPath, "error", err)
		writeWebDAVError(w, err)
		return
	}
	file, err := h.Uploads.Complete(ctx, userID, uuid.UUID(session.ID.Bytes))
	if err != nil {
		writeWebDAVError(w, err)
		return
	}
	_ = spool
	w.Header().Set("ETag", fileETag(file))
	status := http.StatusCreated
	if existing != nil {
		status = http.StatusNoContent
	}
	writeStatus(w, status)
}

func (h *Handler) storeUpload(ctx context.Context, userID int64, session *sqlcgen.UploadSession, reader io.Reader, size int64) error {
	if size == 0 {
		return nil
	}
	partSize := session.PartSize
	if partSize <= 0 {
		partSize = putPartSize
	}
	partNo := int32(1)
	var offset int64
	for offset < size {
		length := min(partSize, size-offset)
		limited := io.LimitReader(reader, length)
		_, err := h.Pipeline.UploadPart(ctx, transfer.UploadPartRequest{
			UserID: userID, UploadID: uuid.UUID(session.ID.Bytes), PartNo: partNo,
			PlainSize: length, Body: limited,
		})
		if err != nil {
			return err
		}
		offset += length
		partNo++
	}
	return nil
}

func (h *Handler) mkcol(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64, requestPath string) {
	if requestPath == "/" {
		writeStatus(w, http.StatusMethodNotAllowed)
		return
	}
	if _, err := h.resolve(ctx, userID, requestPath); err == nil {
		writeStatus(w, http.StatusMethodNotAllowed)
		return
	}
	parentPath, name := splitPath(requestPath)
	parent, err := h.resolve(ctx, userID, parentPath)
	if err != nil || (parent != nil && parent.Kind != sqlcgen.FileKindFolder) {
		writeStatus(w, http.StatusConflict)
		return
	}
	var parentID *uuid.UUID
	if parent != nil {
		id, ok := dbtypes.GoogleUUID(parent.ID)
		if !ok {
			writeStatus(w, http.StatusInternalServerError)
			return
		}
		parentID = &id
	}
	if _, err := h.Catalog.CreateFolder(ctx, catalog.CreateFolderInput{UserID: userID, ParentID: parentID, Name: name, ModTime: time.Now().UTC()}); err != nil {
		writeWebDAVError(w, err)
		return
	}
	writeStatus(w, http.StatusCreated)
}

func (h *Handler) delete(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64, requestPath string) {
	file, err := h.resolve(ctx, userID, requestPath)
	if err != nil || file == nil {
		writeStatus(w, http.StatusNotFound)
		return
	}
	fileID, ok := dbtypes.GoogleUUID(file.ID)
	if !ok {
		writeStatus(w, http.StatusInternalServerError)
		return
	}
	if _, err := h.Catalog.Trash(ctx, userID, fileID); err != nil {
		writeWebDAVError(w, err)
		return
	}
	writeStatus(w, http.StatusNoContent)
}

func (h *Handler) move(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64, requestPath string) {
	source, err := h.resolve(ctx, userID, requestPath)
	if err != nil || source == nil {
		writeStatus(w, http.StatusNotFound)
		return
	}
	sourceID, ok := dbtypes.GoogleUUID(source.ID)
	if !ok {
		writeStatus(w, http.StatusInternalServerError)
		return
	}
	destinationPath, err := h.destinationPath(r)
	if err != nil {
		writeStatus(w, http.StatusBadRequest)
		return
	}
	target, _ := h.resolve(ctx, userID, destinationPath)
	overwrite := strings.EqualFold(r.Header.Get("Overwrite"), "T") || r.Header.Get("Overwrite") == ""
	if target != nil {
		if !overwrite {
			writeStatus(w, http.StatusPreconditionFailed)
			return
		}
		targetID, ok := dbtypes.GoogleUUID(target.ID)
		if ok {
			if _, err := h.Catalog.Trash(ctx, userID, targetID); err != nil {
				writeWebDAVError(w, err)
				return
			}
		}
	}
	parentPath, name := splitPath(destinationPath)
	parent, err := h.resolve(ctx, userID, parentPath)
	if err != nil || (parent != nil && parent.Kind != sqlcgen.FileKindFolder) {
		writeStatus(w, http.StatusConflict)
		return
	}
	var parentID *uuid.UUID
	if parent != nil {
		id, ok := dbtypes.GoogleUUID(parent.ID)
		if !ok {
			writeStatus(w, http.StatusInternalServerError)
			return
		}
		parentID = &id
	}
	sameParent := (source.ParentID.Valid && parentID != nil && source.ParentID.Bytes == *parentID) || (!source.ParentID.Valid && parentID == nil)
	if !sameParent || source.Name != name {
		if source.Name != name {
			if _, err := h.Catalog.Rename(ctx, userID, sourceID, nil, name); err != nil {
				writeWebDAVError(w, err)
				return
			}
		}
		if !sameParent {
			if _, err := h.Catalog.Move(ctx, userID, sourceID, parentID, nil); err != nil {
				writeWebDAVError(w, err)
				return
			}
		}
	}
	if target != nil {
		writeStatus(w, http.StatusNoContent)
		return
	}
	writeStatus(w, http.StatusCreated)
}

func (h *Handler) copy(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64, requestPath string) {
	source, err := h.resolve(ctx, userID, requestPath)
	if err != nil || source == nil {
		writeStatus(w, http.StatusNotFound)
		return
	}
	sourceID, ok := dbtypes.GoogleUUID(source.ID)
	if !ok {
		writeStatus(w, http.StatusInternalServerError)
		return
	}
	destinationPath, err := h.destinationPath(r)
	if err != nil {
		writeStatus(w, http.StatusBadRequest)
		return
	}
	target, _ := h.resolve(ctx, userID, destinationPath)
	overwrite := strings.EqualFold(r.Header.Get("Overwrite"), "T") || r.Header.Get("Overwrite") == ""
	if target != nil && !overwrite {
		writeStatus(w, http.StatusPreconditionFailed)
		return
	}
	parentPath, name := splitPath(destinationPath)
	parent, err := h.resolve(ctx, userID, parentPath)
	if err != nil || (parent != nil && parent.Kind != sqlcgen.FileKindFolder) {
		writeStatus(w, http.StatusConflict)
		return
	}
	var parentID *uuid.UUID
	if parent != nil {
		id, ok := dbtypes.GoogleUUID(parent.ID)
		if !ok {
			writeStatus(w, http.StatusInternalServerError)
			return
		}
		parentID = &id
	}
	policy := sqlcgen.NameConflictPolicyFail
	if target != nil {
		policy = sqlcgen.NameConflictPolicyReplace
	}
	if _, err := h.Files.Copy(ctx, fileops.CopyInput{UserID: userID, FileID: sourceID, ParentID: parentID, Name: &name, ConflictPolicy: policy}); err != nil {
		writeWebDAVError(w, err)
		return
	}
	if target != nil {
		writeStatus(w, http.StatusNoContent)
		return
	}
	writeStatus(w, http.StatusCreated)
}

// ---- helpers -----------------------------------------------------------

func (h *Handler) destinationPath(r *http.Request) (string, error) {
	raw := strings.TrimSpace(r.Header.Get("Destination"))
	if raw == "" {
		return "", ErrInvalid
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", ErrInvalid
	}
	decoded, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil {
		return "", ErrInvalid
	}
	prefix := strings.TrimSuffix(h.Prefix, "/")
	if prefix != "" {
		decoded = strings.TrimPrefix(decoded, prefix)
	}
	return normalizePath(decoded)
}

func (h *Handler) resolve(ctx context.Context, userID int64, requestPath string) (*sqlcgen.File, error) {
	if requestPath == "/" {
		return nil, nil
	}
	var parent *uuid.UUID
	components := strings.Split(strings.Trim(requestPath, "/"), "/")
	var current *sqlcgen.File
	for _, component := range components {
		if component == "" {
			return nil, ErrNotFound
		}
		children, err := h.listChildren(ctx, userID, parent)
		if err != nil {
			return nil, err
		}
		var found *sqlcgen.File
		for _, child := range children {
			if child.Name == component {
				found = child
				break
			}
		}
		if found == nil {
			return nil, ErrNotFound
		}
		current = found
		id, ok := dbtypes.GoogleUUID(found.ID)
		if !ok {
			return nil, ErrNotFound
		}
		parent = &id
	}
	return current, nil
}

func (h *Handler) listChildren(ctx context.Context, userID int64, parentID *uuid.UUID) ([]*sqlcgen.File, error) {
	var items []*sqlcgen.File
	for {
		input := catalog.ListInput{
			UserID: userID, ParentID: parentID, Status: sqlcgen.FileStatusActive,
			Limit: 500, Sort: "name", Order: "asc",
		}
		if len(items) > 0 {
			last := items[len(items)-1]
			lastID, ok := dbtypes.GoogleUUID(last.ID)
			if !ok {
				return nil, ErrNotFound
			}
			input.AfterName, input.AfterID = last.Name, &lastID
		}
		page, err := h.Catalog.List(ctx, input)
		if err != nil {
			return nil, err
		}
		items = append(items, page...)
		if len(page) < 500 {
			return items, nil
		}
	}
}

func normalizePath(raw string) (string, error) {
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", ErrInvalid
	}
	if strings.Contains(decoded, "\x00") {
		return "", ErrInvalid
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(decoded, "/"))
	if strings.Contains(cleaned, "..") {
		return "", ErrInvalid
	}
	return cleaned, nil
}

func splitPath(requestPath string) (string, string) {
	parent, name := path.Split(strings.TrimSuffix(requestPath, "/"))
	return path.Clean("/" + parent), name
}

func parseRange(header string, size int64) (start, length int64, partial, satisfiable bool) {
	if size < 0 {
		size = 0
	}
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, size, false, true
	}
	if !strings.HasPrefix(header, "bytes=") {
		return 0, size, false, true
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, size, false, true // multi-range: serve the whole entity
	}
	rangeStart, rangeEnd, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, size, false, true
	}
	if rangeStart == "" {
		suffix, err := strconv.ParseInt(strings.TrimSpace(rangeEnd), 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, false
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, suffix, true, true
	}
	startValue, err := strconv.ParseInt(strings.TrimSpace(rangeStart), 10, 64)
	if err != nil || startValue < 0 || startValue >= size {
		return 0, 0, false, false
	}
	end := size - 1
	if strings.TrimSpace(rangeEnd) != "" {
		endValue, err := strconv.ParseInt(strings.TrimSpace(rangeEnd), 10, 64)
		if err != nil || endValue < startValue {
			return 0, 0, false, false
		}
		if endValue < end {
			end = endValue
		}
	}
	return startValue, end - startValue + 1, true, true
}

func detectMimeType(name, header string) string {
	contentType := strings.TrimSpace(strings.Split(header, ";")[0])
	if contentType != "" && contentType != "application/octet-stream" {
		return contentType
	}
	if guessed := mime.TypeByExtension(path.Ext(name)); guessed != "" {
		return guessed
	}
	return "application/octet-stream"
}

func fileETag(file *sqlcgen.File) string {
	id, _ := dbtypes.GoogleUUID(file.ID)
	return fmt.Sprintf(`"%s-%d"`, id, file.Generation)
}

func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func writeWebDAVError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, catalog.ErrNotFound):
		writeStatus(w, http.StatusNotFound)
	case errors.Is(err, ErrConflict), errors.Is(err, uploads.ErrNameConflict):
		writeStatus(w, http.StatusConflict)
	case errors.Is(err, ErrInvalid), errors.Is(err, catalog.ErrInvalidParent):
		writeStatus(w, http.StatusBadRequest)
	default:
		writeStatus(w, http.StatusInternalServerError)
	}
}

func writeStatus(w http.ResponseWriter, status int) {
	w.WriteHeader(status)
}
