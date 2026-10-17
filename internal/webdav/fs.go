package webdav

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/webdav"

	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
)

// listPageSize is the largest catalog page.
const listPageSize = 500

var errWriteUnsupported = errors.New("WebDAV writes go through PUT")

// requestState carries the user and the entries resolved during one request.
// x/net/webdav stats every child of a listed folder by path, so without it a
// PROPFIND of a folder would resolve each child from the root again.
type requestState struct {
	userID  int64
	mu      sync.Mutex
	entries map[string]*entry
}

type requestStateKey struct{}

func withRequestState(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, requestStateKey{}, &requestState{userID: userID, entries: make(map[string]*entry)})
}

func stateFrom(ctx context.Context) *requestState {
	state, _ := ctx.Value(requestStateKey{}).(*requestState)
	return state
}

// lookup returns a cached entry. A nil entry with ok set means the path is
// known not to exist.
func (s *requestState) lookup(name string) (*entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cached, ok := s.entries[name]
	return cached, ok
}

func (s *requestState) store(name string, value *entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[name] = value
}

// forget drops name and everything below it.
func (s *requestState) forget(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.entries {
		if key == name || strings.HasPrefix(key, name+"/") {
			delete(s.entries, key)
		}
	}
}

// entry is a resolved path: the drive root or a catalog file or folder.
type entry struct {
	path string
	file *sqlcgen.File
}

func (e *entry) isRoot() bool { return e.file == nil }

func (e *entry) isDir() bool { return e.file == nil || e.file.Kind == sqlcgen.FileKindFolder }

// id returns the catalog ID, nil for the root.
func (e *entry) id() *uuid.UUID {
	if e.file == nil {
		return nil
	}
	id, ok := dbtypes.GoogleUUID(e.file.ID)
	if !ok {
		return nil
	}
	return &id
}

func (e *entry) size() int64 {
	if e.isDir() || !e.file.Size.Valid {
		return 0
	}
	return e.file.Size.Int64
}

func (e *entry) modTime(fallback time.Time) time.Time {
	switch {
	case e.file == nil:
		return fallback
	case e.file.ModTime.Valid:
		return e.file.ModTime.Time
	default:
		return e.file.UpdatedAt.Time
	}
}

func (e *entry) etag() string {
	switch {
	case e.file == nil:
		return `"root"`
	case e.file.Kind == sqlcgen.FileKindFile && e.file.HashValue.Valid && strings.TrimSpace(e.file.HashValue.String) != "":
		return `"` + e.file.HashValue.String + `"`
	default:
		id, _ := dbtypes.GoogleUUID(e.file.ID)
		return `"` + id.String() + "-" + strconv.FormatInt(e.file.Generation, 10) + `"`
	}
}

func (e *entry) contentType() string {
	if e.isDir() {
		return "httpd/unix-directory"
	}
	return fileContentType(e.file.Name, e.file.MimeType.String)
}

func fileContentType(name, stored string) string {
	if value := strings.TrimSpace(stored); value != "" {
		return value
	}
	if value := mime.TypeByExtension(path.Ext(name)); value != "" {
		return value
	}
	return "application/octet-stream"
}

func cleanPath(name string) string {
	return path.Clean("/" + name)
}

func notExist(op, name string) error {
	return &os.PathError{Op: op, Path: name, Err: os.ErrNotExist}
}

// fileSystem adapts the catalog to x/net/webdav. The user comes from the
// request state in the context.
type fileSystem struct {
	handler *Handler
	started time.Time
}

func (fsys *fileSystem) state(ctx context.Context) (*requestState, error) {
	state := stateFrom(ctx)
	if state == nil || state.userID <= 0 {
		return nil, os.ErrPermission
	}
	return state, nil
}

func (fsys *fileSystem) resolve(ctx context.Context, name string) (*entry, error) {
	state, err := fsys.state(ctx)
	if err != nil {
		return nil, err
	}
	return fsys.resolveClean(ctx, state, cleanPath(name))
}

func (fsys *fileSystem) resolveClean(ctx context.Context, state *requestState, name string) (*entry, error) {
	if name == "/" {
		return &entry{path: "/"}, nil
	}
	if cached, ok := state.lookup(name); ok {
		if cached == nil {
			return nil, notExist("stat", name)
		}
		return cached, nil
	}
	dir, base := path.Split(name)
	parent, err := fsys.resolveClean(ctx, state, cleanPath(dir))
	if err != nil {
		return nil, err
	}
	if !parent.isDir() {
		return nil, notExist("stat", name)
	}
	file, err := fsys.handler.svc.Catalog.ResolveChild(ctx, state.userID, parent.id(), base)
	if errors.Is(err, catalog.ErrNotFound) {
		state.store(name, nil)
		return nil, notExist("stat", name)
	}
	if err != nil {
		return nil, err
	}
	resolved := &entry{path: name, file: file}
	state.store(name, resolved)
	return resolved, nil
}

// resolveParent resolves the folder that holds name and returns the base name.
// A missing parent is reported as os.ErrNotExist, which WebDAV answers with
// 409 Conflict.
func (fsys *fileSystem) resolveParent(ctx context.Context, name string) (*entry, string, error) {
	clean := cleanPath(name)
	if clean == "/" {
		return nil, "", os.ErrPermission
	}
	dir, base := path.Split(clean)
	parent, err := fsys.resolve(ctx, dir)
	if err != nil {
		return nil, "", err
	}
	if !parent.isDir() {
		return nil, "", notExist("stat", dir)
	}
	return parent, base, nil
}

// list returns the children of a folder and caches them for the request.
// Children whose names contain a slash cannot be addressed by path and are
// left out.
func (fsys *fileSystem) list(ctx context.Context, dir *entry) ([]*entry, error) {
	state, err := fsys.state(ctx)
	if err != nil {
		return nil, err
	}
	var children []*entry
	input := catalog.ListInput{UserID: state.userID, ParentID: dir.id(), Limit: listPageSize}
	for {
		items, err := fsys.handler.svc.Catalog.List(ctx, input)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if strings.Contains(item.Name, "/") {
				continue
			}
			child := &entry{path: path.Join(dir.path, item.Name), file: item}
			state.store(child.path, child)
			children = append(children, child)
		}
		if len(items) < listPageSize {
			return children, nil
		}
		last := items[len(items)-1]
		lastID, ok := dbtypes.GoogleUUID(last.ID)
		if !ok {
			return children, nil
		}
		input.AfterName, input.AfterID = last.Name, &lastID
	}
}

func (fsys *fileSystem) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	state, err := fsys.state(ctx)
	if err != nil {
		return err
	}
	if _, err := fsys.resolve(ctx, name); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent, base, err := fsys.resolveParent(ctx, name)
	if err != nil {
		return err
	}
	folder, err := fsys.handler.svc.Catalog.CreateFolder(ctx, catalog.CreateFolderInput{UserID: state.userID, ParentID: parent.id(), Name: base})
	if errors.Is(err, catalog.ErrConflict) {
		return os.ErrExist
	}
	if err != nil {
		return err
	}
	clean := cleanPath(name)
	state.forget(clean)
	state.store(clean, &entry{path: clean, file: folder})
	return nil
}

func (fsys *fileSystem) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	resolved, err := fsys.resolve(ctx, name)
	if errors.Is(err, os.ErrNotExist) && flag&os.O_CREATE != 0 {
		// LOCK on an unmapped URL creates an empty resource.
		resolved, err = fsys.createEmpty(ctx, name)
	}
	if err != nil {
		return nil, err
	}
	return &davFile{fsys: fsys, ctx: ctx, entry: resolved}, nil
}

// createEmpty creates an empty file at name.
func (fsys *fileSystem) createEmpty(ctx context.Context, name string) (*entry, error) {
	state, err := fsys.state(ctx)
	if err != nil {
		return nil, err
	}
	if fsys.handler.cfg.ReadOnly {
		return nil, os.ErrPermission
	}
	parent, base, err := fsys.resolveParent(ctx, name)
	if err != nil {
		return nil, err
	}
	file, err := fsys.handler.createFile(ctx, state.userID, parent, base, nil, 0, nil, time.Time{}, sqlcgen.NameConflictPolicyFail)
	if err != nil {
		return nil, err
	}
	clean := cleanPath(name)
	created := &entry{path: clean, file: file}
	state.store(clean, created)
	return created, nil
}

// RemoveAll moves the entry to the TelDrive trash, from which it can be
// restored.
func (fsys *fileSystem) RemoveAll(ctx context.Context, name string) error {
	state, err := fsys.state(ctx)
	if err != nil {
		return err
	}
	resolved, err := fsys.resolve(ctx, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if resolved.isRoot() {
		return os.ErrPermission
	}
	if _, err := fsys.handler.svc.Catalog.Trash(ctx, state.userID, *resolved.id()); err != nil {
		return err
	}
	state.forget(resolved.path)
	return nil
}

func (fsys *fileSystem) Rename(ctx context.Context, oldName, newName string) error {
	source, err := fsys.resolve(ctx, oldName)
	if err != nil {
		return err
	}
	parent, base, err := fsys.resolveParent(ctx, newName)
	if err != nil {
		return err
	}
	return fsys.handler.move(ctx, source, parent, base)
}

func (fsys *fileSystem) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	resolved, err := fsys.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return fileInfo{entry: resolved, rootTime: fsys.started}, nil
}

// fileInfo describes an entry. It implements webdav.ContentTyper and
// webdav.ETager: without them x/net/webdav would read the start of every
// listed file from Telegram to sniff its content type.
type fileInfo struct {
	entry    *entry
	rootTime time.Time
}

func (fi fileInfo) Name() string {
	if fi.entry.isRoot() {
		return "/"
	}
	return fi.entry.file.Name
}

func (fi fileInfo) Size() int64 { return fi.entry.size() }

func (fi fileInfo) Mode() fs.FileMode {
	if fi.entry.isDir() {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

func (fi fileInfo) ModTime() time.Time { return fi.entry.modTime(fi.rootTime) }

func (fi fileInfo) IsDir() bool { return fi.entry.isDir() }

func (fi fileInfo) Sys() any { return nil }

func (fi fileInfo) ContentType(context.Context) (string, error) { return fi.entry.contentType(), nil }

func (fi fileInfo) ETag(context.Context) (string, error) { return fi.entry.etag(), nil }

// davFile is an open entry. Reads open a download lazily, so listing and
// property requests never touch Telegram.
type davFile struct {
	fsys   *fileSystem
	ctx    context.Context
	entry  *entry
	reader *lazyReader
}

func (f *davFile) Close() error {
	if f.reader != nil {
		return f.reader.Close()
	}
	return nil
}

func (f *davFile) content() (*lazyReader, error) {
	if f.entry.isDir() {
		return nil, os.ErrInvalid
	}
	if f.reader == nil {
		state, err := f.fsys.state(f.ctx)
		if err != nil {
			return nil, err
		}
		f.reader = newLazyReader(f.ctx, f.fsys.handler.svc.Downloader, state.userID, f.entry)
	}
	return f.reader, nil
}

func (f *davFile) Read(p []byte) (int, error) {
	reader, err := f.content()
	if err != nil {
		return 0, err
	}
	return reader.Read(p)
}

func (f *davFile) Seek(offset int64, whence int) (int64, error) {
	reader, err := f.content()
	if err != nil {
		return 0, err
	}
	return reader.Seek(offset, whence)
}

func (f *davFile) Readdir(count int) ([]fs.FileInfo, error) {
	if !f.entry.isDir() {
		return nil, os.ErrInvalid
	}
	children, err := f.fsys.list(f.ctx, f.entry)
	if err != nil {
		return nil, err
	}
	if count > 0 && len(children) > count {
		children = children[:count]
	}
	infos := make([]fs.FileInfo, len(children))
	for i, child := range children {
		infos[i] = fileInfo{entry: child, rootTime: f.fsys.started}
	}
	if count > 0 && len(infos) == 0 {
		return nil, io.EOF
	}
	return infos, nil
}

func (f *davFile) Stat() (fs.FileInfo, error) {
	return fileInfo{entry: f.entry, rootTime: f.fsys.started}, nil
}

func (f *davFile) Write([]byte) (int, error) { return 0, errWriteUnsupported }

// DeadProps reports no dead properties.
func (f *davFile) DeadProps() (map[xml.Name]webdav.Property, error) { return nil, nil }

// Patch accepts and discards dead properties, such as the Win32 timestamps
// Windows Explorer sets after every upload; rejecting them makes Explorer
// report failed copies.
func (f *davFile) Patch(patches []webdav.Proppatch) ([]webdav.Propstat, error) {
	accepted := webdav.Propstat{Status: http.StatusOK}
	for _, patch := range patches {
		for _, property := range patch.Props {
			accepted.Props = append(accepted.Props, webdav.Property{XMLName: property.XMLName})
		}
	}
	return []webdav.Propstat{accepted}, nil
}

var (
	_ webdav.FileSystem      = (*fileSystem)(nil)
	_ webdav.File            = (*davFile)(nil)
	_ webdav.DeadPropsHolder = (*davFile)(nil)
	_ webdav.ContentTyper    = fileInfo{}
	_ webdav.ETager          = fileInfo{}
)
