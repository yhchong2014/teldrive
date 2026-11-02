package api

import (
	"bytes"
	"context"
	"errors"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

// GetFileThumbnail serves Telegram's existing preview only after applying the
// same owner/grant checks as file downloads. It never persists thumbnail bytes.
func (h *Handler) GetFileThumbnail(ctx context.Context, params gen.GetFileThumbnailParams) (gen.GetFileThumbnailRes, error) {
	if h == nil || h.Catalog == nil || h.Thumbnails == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	id := googleUUID(params.FileId)
	access, err := h.resolveAuthenticatedFileAccess(ctx, id, false)
	if err != nil {
		return nil, mapServiceError(err)
	}
	file, err := h.Catalog.Get(ctx, access.OwnerID, id)
	if err != nil {
		return nil, mapServiceError(err)
	}
	// An encrypted file's preview must not reveal plaintext. Multi-part uploads
	// are not whole Telegram videos and must not show misleading previews.
	if file.Kind != sqlcgen.FileKindFile || file.Status != sqlcgen.FileStatusActive || file.Encryption {
		return nil, mapServiceError(catalog.ErrNotFound)
	}
	parts, err := h.Catalog.Parts(ctx, access.OwnerID, id)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if len(parts) != 1 || parts[0] == nil {
		return nil, mapServiceError(catalog.ErrNotFound)
	}
	data, contentType, err := h.Thumbnails.Thumbnail(ctx, telegramstore.MetadataRequest{
		UserID: access.OwnerID, ChannelID: parts[0].ChannelID, MessageID: parts[0].MessageID,
	})
	if errors.Is(err, telegramstore.ErrThumbnailNotFound) || errors.Is(err, telegramstore.ErrMessageNotFound) || errors.Is(err, telegramstore.ErrDocumentNotFound) {
		return nil, mapServiceError(catalog.ErrNotFound)
	}
	if err != nil {
		return nil, mapServiceError(err)
	}
	switch contentType {
	case "image/jpeg":
		return &gen.GetFileThumbnailOKImageJpegHeaders{CacheControl: gen.GetFileThumbnailOKCacheControlPrivateNoStore, XContentTypeOptions: gen.GetFileThumbnailOKXContentTypeOptionsNosniff, Response: gen.GetFileThumbnailOKImageJpeg{Data: bytes.NewReader(data)}}, nil
	case "image/png":
		return &gen.GetFileThumbnailOKImagePNGHeaders{CacheControl: gen.GetFileThumbnailOKCacheControlPrivateNoStore, XContentTypeOptions: gen.GetFileThumbnailOKXContentTypeOptionsNosniff, Response: gen.GetFileThumbnailOKImagePNG{Data: bytes.NewReader(data)}}, nil
	case "image/webp":
		return &gen.GetFileThumbnailOKImageWEBPHeaders{CacheControl: gen.GetFileThumbnailOKCacheControlPrivateNoStore, XContentTypeOptions: gen.GetFileThumbnailOKXContentTypeOptionsNosniff, Response: gen.GetFileThumbnailOKImageWEBP{Data: bytes.NewReader(data)}}, nil
	default:
		return nil, mapServiceError(catalog.ErrNotFound)
	}
}
