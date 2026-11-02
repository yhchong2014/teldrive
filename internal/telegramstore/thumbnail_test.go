package telegramstore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestSelectThumbnail(t *testing.T) {
	cached := []byte{1, 2, 3}
	tests := []struct {
		name   string
		thumbs []tg.PhotoSizeClass
		want   string
		cached []byte
	}{
		{name: "empty"},
		{name: "largest", thumbs: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "s", W: 90, H: 90, Size: 100}, &tg.PhotoSize{Type: "m", W: 320, H: 180, Size: 200}}, want: "m"},
		{name: "cached", thumbs: []tg.PhotoSizeClass{&tg.PhotoCachedSize{Type: "m", W: 320, H: 180, Bytes: cached}}, want: "m", cached: cached},
		{name: "progressive", thumbs: []tg.PhotoSizeClass{&tg.PhotoSizeProgressive{Type: "m", W: 320, H: 180, Sizes: []int{10, 200}}}, want: "m"},
		{name: "oversized ignored", thumbs: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "x", W: 500, H: 500, Size: maxThumbnailBytes}, &tg.PhotoSize{Type: "s", W: 90, H: 90, Size: 100}}, want: "s"},
		{name: "stripped ignored", thumbs: []tg.PhotoSizeClass{&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{1}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, got := selectThumbnail(tt.thumbs)
			if name != tt.want || !bytes.Equal(got, tt.cached) {
				t.Fatalf("selectThumbnail() = %q, %v; want %q, %v", name, got, tt.want, tt.cached)
			}
		})
	}
}

// thumbnailInvoker supplies only the Telegram calls used by the feature.
type thumbnailInvoker struct {
	thumbs    []tg.PhotoSizeClass
	body      []byte
	downloads int
}

func (i *thumbnailInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch request := input.(type) {
	case *tg.ChannelsGetChannelsRequest:
		output.(*tg.MessagesChatsBox).Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Channel{ID: 55, AccessHash: 9}}}
	case *tg.ChannelsGetMessagesRequest:
		output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 5, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 2, AccessHash: 10, FileReference: []byte{1}, Thumbs: i.thumbs}}}}}
	case *tg.UploadGetFileRequest:
		loc, ok := request.Location.(*tg.InputDocumentFileLocation)
		if !ok || loc.ThumbSize != "m" || request.Offset != 0 || request.Limit != maxThumbnailBytes {
			return errors.New("incorrect thumbnail location")
		}
		i.downloads++
		output.(*tg.UploadFileBox).File = &tg.UploadFile{Bytes: i.body}
	default:
		return errors.New("unexpected request")
	}
	return nil
}

func TestThumbnailFetchesTelegramPreview(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xd9}
	cases := []struct {
		name          string
		thumbs        []tg.PhotoSizeClass
		body          []byte
		wantDownloads int
		wantErr       error
	}{
		{name: "remote", thumbs: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "m", W: 320, H: 180, Size: 4}}, body: jpeg, wantDownloads: 1},
		{name: "cached", thumbs: []tg.PhotoSizeClass{&tg.PhotoCachedSize{Type: "m", W: 320, H: 180, Bytes: jpeg}}},
		{name: "no thumbnail", wantErr: ErrThumbnailNotFound},
		{name: "invalid image", thumbs: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "m", W: 320, H: 180, Size: 4}}, body: []byte("nope"), wantDownloads: 1, wantErr: ErrThumbnailNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invoker := &thumbnailInvoker{thumbs: tc.thumbs, body: tc.body}
			storage := &GotdStorage{runner: channelSelectionRunner{api: tg.NewClient(invoker)}}
			got, contentType, err := storage.Thumbnail(context.Background(), MetadataRequest{UserID: 7, ChannelID: 55, MessageID: 5})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && (!bytes.Equal(got, jpeg) || contentType != "image/jpeg") {
				t.Fatalf("data = %v, content type = %s", got, contentType)
			}
			if invoker.downloads != tc.wantDownloads {
				t.Fatalf("downloads = %d, want %d", invoker.downloads, tc.wantDownloads)
			}
		})
	}
}

func TestThumbnailSlotWaitRespectsCancellation(t *testing.T) {
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	storage := &GotdStorage{runner: channelSelectionRunner{api: tg.NewClient(&thumbnailInvoker{})}, thumbnailSlots: slots}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := storage.Thumbnail(ctx, MetadataRequest{UserID: 7, ChannelID: 55, MessageID: 5})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
}
