package webdav

import (
	"context"
	"encoding/xml"
	"html"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
)

type multiStatus struct {
	XMLName   xml.Name       `xml:"multistatus"`
	XMLNS     string         `xml:"xmlns,attr"`
	Responses []propResponse `xml:"response"`
}

type propResponse struct {
	Href     string     `xml:"href"`
	Propstat []propstat `xml:"propstat"`
}

type propstat struct {
	Status string `xml:"status"`
	Prop   prop   `xml:"prop"`
}

type prop struct {
	DisplayName    string        `xml:"displayname"`
	ResourceType   *resourceType `xml:"resourcetype,omitempty"`
	ContentLength  *int64        `xml:"getcontentlength,omitempty"`
	ContentType    string        `xml:"getcontenttype,omitempty"`
	ETag           string        `xml:"getetag,omitempty"`
	LastModified   string        `xml:"getlastmodified,omitempty"`
	CreationDate   string        `xml:"creationdate,omitempty"`
	SupportedLock  *struct{}     `xml:"supportedlock,omitempty"`
	LockDiscovery  *struct{}     `xml:"lockdiscovery,omitempty"`
	QuotaAvailable *int64        `xml:"quota-available-bytes,omitempty"`
	QuotaUsed      *int64        `xml:"quota-used-bytes,omitempty"`
}

type resourceType struct {
	Collection *struct{} `xml:"collection,omitempty"`
}

type lockDiscovery struct {
	XMLName  xml.Name `xml:"prop"`
	LockInfo lockInfo `xml:"lockdiscovery"`
}

type lockInfo struct {
	ActiveLock activeLock `xml:"activelock"`
}

type activeLock struct {
	LockType  lockType  `xml:"locktype"`
	LockScope lockScope `xml:"lockscope"`
	Depth     string    `xml:"depth"`
	Timeout   string    `xml:"timeout"`
	Token     lockToken `xml:"locktoken"`
}

type lockType struct {
	Write *struct{} `xml:"write"`
}

type lockScope struct {
	Exclusive *struct{} `xml:"exclusive"`
}

type lockToken struct {
	Href string `xml:"href"`
}

func (h *Handler) fileProps(file *sqlcgen.File) prop {
	isFolder := file.Kind == sqlcgen.FileKindFolder
	result := prop{
		DisplayName:   file.Name,
		ResourceType:  &resourceType{},
		ETag:          fileETag(file),
		LastModified:  file.ModTime.Time.UTC().Format(http.TimeFormat),
		CreationDate:  file.CreatedAt.Time.UTC().Format(time.RFC3339),
		SupportedLock: &struct{}{},
		LockDiscovery: &struct{}{},
	}
	if isFolder {
		result.ResourceType.Collection = &struct{}{}
	} else {
		if file.Size.Valid {
			size := file.Size.Int64
			result.ContentLength = &size
		}
		if file.MimeType.Valid && file.MimeType.String != "" {
			result.ContentType = file.MimeType.String
		} else {
			result.ContentType = "application/octet-stream"
		}
	}
	return result
}

func (h *Handler) proppatch(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64, requestPath string) {
	file, err := h.resolve(ctx, userID, requestPath)
	if err != nil || (file == nil && requestPath != "/") {
		writeStatus(w, http.StatusNotFound)
		return
	}
	isFolder := file == nil || file.Kind == sqlcgen.FileKindFolder
	response := multiStatus{
		XMLNS: "DAV:",
		Responses: []propResponse{{
			Href: html.EscapeString(h.href(requestPath, isFolder)),
			Propstat: []propstat{{
				Status: "HTTP/1.1 200 OK",
				Prop:   prop{DisplayName: path.Base(requestPath)},
			}},
		}},
	}
	writeXML(w, http.StatusMultiStatus, response)
}

func (h *Handler) lock(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64, requestPath string) {
	token := "opaquelocktoken:" + uuid.NewString()
	response := lockDiscovery{LockInfo: lockInfo{ActiveLock: activeLock{
		LockType: lockType{Write: &struct{}{}}, LockScope: lockScope{Exclusive: &struct{}{}},
		Depth: "infinity", Timeout: "Second-3600",
		Token: lockToken{Href: token},
	}}}
	w.Header().Set("Lock-Token", "<"+token+">")
	writeXML(w, http.StatusOK, response)
}

// href renders the absolute WebDAV path, including the configured mount
// prefix, so clients can follow the returned links verbatim.
func (h *Handler) href(requestPath string, isFolder bool) string {
	trimmed := strings.TrimPrefix(path.Clean("/"+requestPath), "/")
	escaped := "/" + escapeSegments(trimmed)
	if isFolder && !strings.HasSuffix(escaped, "/") {
		escaped += "/"
	}
	if !strings.HasPrefix(escaped, "/") {
		escaped = "/" + escaped
	}
	return strings.TrimSuffix(h.Prefix, "/") + escaped
}

func escapeSegments(value string) string {
	if value == "" {
		return ""
	}
	segments := strings.Split(value, "/")
	for index, segment := range segments {
		segments[index] = urlPathEscape(segment)
	}
	return strings.Join(segments, "/")
}

func urlPathEscape(segment string) string {
	return strings.NewReplacer(
		"%", "%25", " ", "%20", "#", "%23", "?", "%3F", "&", "%26",
		"<", "%3C", ">", "%3E", `"`, "%22", "'", "%27", "\\", "%5C",
	).Replace(segment)
}

func writeXML(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	encoder := xml.NewEncoder(w)
	encoder.Indent("", "  ")
	_ = encoder.Encode(payload)
	_ = encoder.Flush()
}
