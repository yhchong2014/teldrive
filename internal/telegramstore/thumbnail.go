package telegramstore

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/gotd/td/tg"
)

var ErrThumbnailNotFound = errors.New("Telegram thumbnail not found")

// ThumbnailReader is an optional storage capability. Thumbnail bytes come from
// Telegram and are held in memory only; implementations must not persist them.
// The caller must authorize access to the file before resolving its message.
type ThumbnailReader interface {
	Thumbnail(context.Context, MetadataRequest) ([]byte, string, error)
}

const maxThumbnailBytes = 1024 * 1024

// Thumbnail fetches a document's existing static thumbnail, if Telegram has
// one. Stripped placeholders and animated/video thumbnails are not images that
// can be served directly to browsers and are deliberately ignored.
func (s *GotdStorage) Thumbnail(ctx context.Context, request MetadataRequest) ([]byte, string, error) {
	if s == nil || s.runner == nil || request.UserID <= 0 || request.ChannelID == 0 || request.MessageID <= 0 {
		return nil, "", ErrInvalidRequest
	}
	// Browsing a large folder must not start an unbounded number of Telegram
	// sessions at once. Waiting is cancellable when the browser navigates away.
	if s.thumbnailSlots != nil {
		select {
		case s.thumbnailSlots <- struct{}{}:
			defer func() { <-s.thumbnailSlots }()
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	var data []byte
	// Channel imports are resolved with the owner's user account. A rotating
	// download bot may not have access to the original imported message.
	err := s.runner.Run(ctx, request.UserID, OperationManage, func(ctx context.Context, api *tg.Client) error {
		channel, err := inputChannel(ctx, api, request.ChannelID)
		if err != nil {
			return err
		}
		response, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: channel, ID: []tg.InputMessageClass{&tg.InputMessageID{ID: int(request.MessageID)}},
		})
		if err != nil {
			return fmt.Errorf("get Telegram thumbnail message: %w", err)
		}
		modified, ok := response.AsModified()
		if !ok || len(modified.GetMessages()) == 0 {
			return ErrMessageNotFound
		}
		message, ok := modified.GetMessages()[0].(*tg.Message)
		if !ok {
			return ErrMessageNotFound
		}
		document, ok := messageDocument(message)
		if !ok {
			return ErrDocumentNotFound
		}
		size, cached := selectThumbnail(document.Thumbs)
		if cached != nil {
			data = cached
			return nil
		}
		if size == "" {
			return ErrThumbnailNotFound
		}
		result, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: document.AsInputDocumentFileLocation(size), Offset: 0, Limit: maxThumbnailBytes,
		})
		if err != nil {
			return fmt.Errorf("download Telegram thumbnail: %w", err)
		}
		file, ok := result.(*tg.UploadFile)
		if !ok || len(file.Bytes) == 0 || len(file.Bytes) >= maxThumbnailBytes {
			return ErrThumbnailNotFound
		}
		data = file.Bytes
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	contentType := http.DetectContentType(data)
	if contentType != "image/jpeg" && contentType != "image/png" && contentType != "image/webp" {
		return nil, "", ErrThumbnailNotFound
	}
	return data, contentType, nil
}

// Prefer the largest available static preview while bounding the size of data
// returned by Telegram. Cached sizes already contain their image bytes.
func selectThumbnail(thumbs []tg.PhotoSizeClass) (string, []byte) {
	var selected string
	var cached []byte
	bestArea := int64(0)
	for _, thumb := range thumbs {
		var name string
		var width, height int
		var bytes []byte
		switch size := thumb.(type) {
		case *tg.PhotoSize:
			if size == nil || size.Size <= 0 || size.Size >= maxThumbnailBytes {
				continue
			}
			name, width, height = size.Type, size.W, size.H
		case *tg.PhotoSizeProgressive:
			if size == nil || len(size.Sizes) == 0 || size.Sizes[len(size.Sizes)-1] <= 0 || size.Sizes[len(size.Sizes)-1] >= maxThumbnailBytes {
				continue
			}
			name, width, height = size.Type, size.W, size.H
		case *tg.PhotoCachedSize:
			if size == nil || len(size.Bytes) == 0 || len(size.Bytes) >= maxThumbnailBytes {
				continue
			}
			name, width, height, bytes = size.Type, size.W, size.H, size.Bytes
		default:
			continue
		}
		if name == "" || width <= 0 || height <= 0 || width > 10000 || height > 10000 {
			continue
		}
		area := int64(width) * int64(height)
		if area > bestArea {
			bestArea, selected, cached = area, name, bytes
		}
	}
	return selected, cached
}
