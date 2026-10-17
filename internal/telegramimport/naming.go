package telegramimport

import (
	"fmt"
	"mime"
	"path"
	"strings"
	"unicode"

	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

// maxNameRunes bounds generated file names.
const maxNameRunes = 200

// commonExtensions maps the MIME types Telegram reports most often to their
// usual extension. mime.ExtensionsByType sorts alphabetically, which would
// name MP4 videos .m4v.
var commonExtensions = map[string]string{
	"video/mp4":                    ".mp4",
	"video/x-matroska":             ".mkv",
	"video/webm":                   ".webm",
	"video/quicktime":              ".mov",
	"video/x-msvideo":              ".avi",
	"video/mp2t":                   ".ts",
	"audio/mpeg":                   ".mp3",
	"audio/mp4":                    ".m4a",
	"audio/ogg":                    ".ogg",
	"audio/flac":                   ".flac",
	"audio/x-flac":                 ".flac",
	"image/jpeg":                   ".jpg",
	"image/png":                    ".png",
	"image/webp":                   ".webp",
	"image/gif":                    ".gif",
	"application/pdf":              ".pdf",
	"application/zip":              ".zip",
	"application/x-rar-compressed": ".rar",
	"application/vnd.rar":          ".rar",
	"application/x-7z-compressed":  ".7z",
	"application/x-tgsticker":      ".tgs",
	"application/epub+zip":         ".epub",
}

// DocumentName returns the TelDrive file name for an imported document: its
// Telegram file name, else the first caption line, else kind_messageID. Names
// that are not the document's own file name get an extension from its MIME
// type.
func DocumentName(document telegramstore.ImportDocument) string {
	if name := sanitizeName(document.FileName); name != "" {
		return name
	}
	name := sanitizeName(document.Caption)
	if name == "" {
		kind := document.Kind
		if kind == "" {
			kind = "document"
		}
		name = fmt.Sprintf("%s_%d", kind, document.MessageID)
	}
	if ext := extensionFor(document.MIMEType); ext != "" && !strings.EqualFold(path.Ext(name), ext) {
		name += ext
	}
	return name
}

func extensionFor(mimeType string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(mimeType))
	if err != nil || mediaType == "" {
		return ""
	}
	if ext, ok := commonExtensions[mediaType]; ok {
		return ext
	}
	if extensions, err := mime.ExtensionsByType(mediaType); err == nil && len(extensions) > 0 {
		return extensions[0]
	}
	return ""
}

// sanitizeName makes a single path segment: separators become underscores,
// control characters and repeated spaces go, and the length is bounded.
func sanitizeName(raw string) string {
	var builder strings.Builder
	lastSpace := false
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r == '/' || r == '\\':
			r = '_'
		case unicode.IsControl(r) || r == unicode.ReplacementChar:
			continue
		case unicode.IsSpace(r):
			if lastSpace {
				continue
			}
			r = ' '
		}
		lastSpace = r == ' '
		builder.WriteRune(r)
	}
	name := strings.TrimSpace(builder.String())
	if runes := []rune(name); len(runes) > maxNameRunes {
		name = strings.TrimSpace(string(runes[:maxNameRunes]))
	}
	if strings.Trim(name, ".") == "" {
		return ""
	}
	return name
}
