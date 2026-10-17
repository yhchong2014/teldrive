package telegramstore

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	// maxImportBatch is the most messages Telegram returns per history page
	// or channels.getMessages call.
	maxImportBatch = 100
	// maxImportCaption bounds the caption line kept for naming documents.
	maxImportCaption = 200
)

// ErrForwardRestricted reports a document in a channel with protected content,
// which Telegram does not allow to be copied.
var ErrForwardRestricted = errors.New("Telegram channel does not allow copying its content")

// ChannelRef names a Telegram channel by ID or public username.
type ChannelRef struct {
	ID       int64
	Username string
}

// ImportChannel is a channel resolved for importing documents from it.
type ImportChannel struct {
	ID         int64
	AccessHash int64
	Title      string
	Username   string
}

// ImportDocument describes a document message that can be imported.
type ImportDocument struct {
	MessageID int64
	Date      time.Time
	// FileName is the document's file name attribute; it can be empty, for
	// example for videos sent as media rather than as files.
	FileName string
	MIMEType string
	Size     int64
	// Kind is video, audio, voice, animation, sticker or document.
	Kind string
	// Caption is the first line of the message text, if any.
	Caption string

	document *tg.InputDocument
}

// ImportHistoryPage is one page of a channel history, newest first.
type ImportHistoryPage struct {
	Documents []ImportDocument
	// NextBeforeID continues the listing below the oldest message of this page.
	NextBeforeID int64
	Exhausted    bool
}

// ImportSession reads and copies documents with the user's own account during
// one import run.
type ImportSession interface {
	ResolveChannel(ctx context.Context, ref ChannelRef) (ImportChannel, error)
	// ListDocuments returns up to limit messages older than beforeID (zero for
	// the newest), keeping only those carrying a document.
	ListDocuments(ctx context.Context, channel ImportChannel, beforeID int64, limit int) (ImportHistoryPage, error)
	// GetDocuments returns the document messages among messageIDs. Missing
	// messages and messages without a document are left out.
	GetDocuments(ctx context.Context, channel ImportChannel, messageIDs []int64) ([]ImportDocument, error)
	// CopyDocument sends the document to the destination channel as a new
	// message without touching the source message.
	CopyDocument(ctx context.Context, channel ImportChannel, document ImportDocument, destinationChannelID int64) (StoredPart, error)
}

// DocumentImporter is an optional storage capability that runs Telegram import
// work on the user's account within one Telegram client session.
type DocumentImporter interface {
	RunImport(ctx context.Context, userID int64, fn func(context.Context, ImportSession) error) error
}

// RunImport runs fn with a session on the user's own account. Bots are not used:
// they cannot read channel history.
func (s *GotdStorage) RunImport(ctx context.Context, userID int64, fn func(context.Context, ImportSession) error) error {
	if s.runner == nil || userID <= 0 || fn == nil {
		return ErrInvalidRequest
	}
	return s.runner.Run(ctx, userID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		return fn(runCtx, &gotdImportSession{api: api, destinations: make(map[int64]*tg.InputChannel)})
	})
}

type gotdImportSession struct {
	api          *tg.Client
	destinations map[int64]*tg.InputChannel
}

func (s *gotdImportSession) ResolveChannel(ctx context.Context, ref ChannelRef) (ImportChannel, error) {
	username := strings.TrimPrefix(strings.TrimSpace(ref.Username), "@")
	if username != "" {
		resolved, err := s.api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
		if err != nil {
			if tgerr.Is(err, "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID") {
				return ImportChannel{}, fmt.Errorf("%w: @%s does not exist", ErrInvalidChannel, username)
			}
			return ImportChannel{}, fmt.Errorf("resolve Telegram username @%s: %w", username, err)
		}
		peer, ok := resolved.Peer.(*tg.PeerChannel)
		if !ok {
			return ImportChannel{}, fmt.Errorf("%w: @%s is not a channel", ErrInvalidChannel, username)
		}
		for _, chat := range resolved.Chats {
			if channel, ok := chat.(*tg.Channel); ok && channel.ID == peer.ChannelID {
				return importChannel(channel), nil
			}
		}
		return ImportChannel{}, fmt.Errorf("%w: @%s is not accessible", ErrInvalidChannel, username)
	}
	if ref.ID <= 0 {
		return ImportChannel{}, ErrInvalidRequest
	}
	response, err := s.api.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: ref.ID}})
	if err != nil {
		if tgerr.Is(err, "CHANNEL_INVALID", "CHANNEL_PRIVATE", "CHANNEL_PUBLIC_GROUP_NA") {
			return ImportChannel{}, fmt.Errorf("%w: channel %d is not accessible to your Telegram account; join it first", ErrInvalidChannel, ref.ID)
		}
		return ImportChannel{}, fmt.Errorf("resolve Telegram channel %d: %w", ref.ID, err)
	}
	for _, chat := range response.GetChats() {
		switch channel := chat.(type) {
		case *tg.Channel:
			if channel.ID == ref.ID {
				return importChannel(channel), nil
			}
		case *tg.ChannelForbidden:
			if channel.ID == ref.ID {
				return ImportChannel{}, fmt.Errorf("%w: access to channel %d is forbidden", ErrInvalidChannel, ref.ID)
			}
		}
	}
	return ImportChannel{}, fmt.Errorf("%w: channel %d was not found", ErrInvalidChannel, ref.ID)
}

func importChannel(channel *tg.Channel) ImportChannel {
	return ImportChannel{ID: channel.ID, AccessHash: channel.AccessHash, Title: channel.Title, Username: channel.Username}
}

func (s *gotdImportSession) ListDocuments(ctx context.Context, channel ImportChannel, beforeID int64, limit int) (ImportHistoryPage, error) {
	if channel.ID <= 0 || beforeID < 0 || limit <= 0 || limit > maxImportBatch {
		return ImportHistoryPage{}, ErrInvalidRequest
	}
	result, err := s.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:     &tg.InputPeerChannel{ChannelID: channel.ID, AccessHash: channel.AccessHash},
		OffsetID: int(beforeID),
		Limit:    limit,
	})
	if err != nil {
		return ImportHistoryPage{}, fmt.Errorf("list Telegram channel history: %w", err)
	}
	modified, ok := result.AsModified()
	if !ok {
		return ImportHistoryPage{}, errors.New("list Telegram channel history: unexpected unmodified response")
	}
	messages := modified.GetMessages()
	page := ImportHistoryPage{Exhausted: len(messages) == 0}
	for _, item := range messages {
		id := int64(item.GetID())
		if id > 0 && (page.NextBeforeID == 0 || id < page.NextBeforeID) {
			page.NextBeforeID = id
		}
		if message, ok := item.(*tg.Message); ok {
			if document, ok := importDocument(message); ok {
				page.Documents = append(page.Documents, document)
			}
		}
	}
	if !page.Exhausted && page.NextBeforeID <= 1 {
		// Message 1 is the oldest a channel can have.
		page.Exhausted = true
	}
	return page, nil
}

func (s *gotdImportSession) GetDocuments(ctx context.Context, channel ImportChannel, messageIDs []int64) ([]ImportDocument, error) {
	if channel.ID <= 0 || len(messageIDs) > maxImportBatch {
		return nil, ErrInvalidRequest
	}
	if len(messageIDs) == 0 {
		return nil, nil
	}
	ids := make([]tg.InputMessageClass, 0, len(messageIDs))
	for _, id := range messageIDs {
		if id <= 0 {
			return nil, ErrInvalidRequest
		}
		ids = append(ids, &tg.InputMessageID{ID: int(id)})
	}
	response, err := s.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: &tg.InputChannel{ChannelID: channel.ID, AccessHash: channel.AccessHash},
		ID:      ids,
	})
	if err != nil {
		return nil, fmt.Errorf("get Telegram channel messages: %w", err)
	}
	modified, ok := response.AsModified()
	if !ok {
		return nil, errors.New("get Telegram channel messages: unexpected unmodified response")
	}
	var documents []ImportDocument
	for _, item := range modified.GetMessages() {
		if message, ok := item.(*tg.Message); ok {
			if document, ok := importDocument(message); ok {
				documents = append(documents, document)
			}
		}
	}
	return documents, nil
}

func (s *gotdImportSession) CopyDocument(ctx context.Context, channel ImportChannel, document ImportDocument, destinationChannelID int64) (StoredPart, error) {
	if channel.ID <= 0 || document.MessageID <= 0 || destinationChannelID == 0 {
		return StoredPart{}, ErrInvalidRequest
	}
	destination, err := s.destination(ctx, destinationChannelID)
	if err != nil {
		return StoredPart{}, err
	}
	input := document.document
	if input == nil {
		if input, err = s.refetch(ctx, channel, document.MessageID); err != nil {
			return StoredPart{}, err
		}
	}
	response, err := s.sendDocument(ctx, destination, input)
	if tgerr.Is(err, "FILE_REFERENCE_EXPIRED", "FILE_REFERENCE_INVALID") {
		// The listing is older than the document's file reference.
		if input, err = s.refetch(ctx, channel, document.MessageID); err != nil {
			return StoredPart{}, err
		}
		response, err = s.sendDocument(ctx, destination, input)
	}
	if err != nil {
		if tgerr.Is(err, "CHAT_FORWARDS_RESTRICTED") {
			return StoredPart{}, ErrForwardRestricted
		}
		return StoredPart{}, fmt.Errorf("copy Telegram document: %w", err)
	}
	messageID, copiedSize, err := uploadedMessage(response)
	if err != nil {
		return StoredPart{}, err
	}
	if copiedSize != document.Size {
		return StoredPart{ChannelID: destinationChannelID, MessageID: messageID, Size: copiedSize},
			fmt.Errorf("%w: copied %d, source %d", ErrSizeMismatch, copiedSize, document.Size)
	}
	return StoredPart{ChannelID: destinationChannelID, MessageID: messageID, Size: copiedSize}, nil
}

func (s *gotdImportSession) sendDocument(ctx context.Context, destination *tg.InputChannel, document *tg.InputDocument) (tg.UpdatesClass, error) {
	var randomBytes [8]byte
	if _, err := cryptorand.Read(randomBytes[:]); err != nil {
		return nil, fmt.Errorf("generate Telegram copy random id: %w", err)
	}
	return s.api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Silent:   true,
		Peer:     &tg.InputPeerChannel{ChannelID: destination.ChannelID, AccessHash: destination.AccessHash},
		Media:    &tg.InputMediaDocument{ID: document},
		RandomID: int64(binary.BigEndian.Uint64(randomBytes[:])),
	})
}

func (s *gotdImportSession) refetch(ctx context.Context, channel ImportChannel, messageID int64) (*tg.InputDocument, error) {
	documents, err := s.GetDocuments(ctx, channel, []int64{messageID})
	if err != nil {
		return nil, err
	}
	if len(documents) == 0 || documents[0].document == nil {
		return nil, ErrDocumentNotFound
	}
	return documents[0].document, nil
}

func (s *gotdImportSession) destination(ctx context.Context, channelID int64) (*tg.InputChannel, error) {
	if destination := s.destinations[channelID]; destination != nil {
		return destination, nil
	}
	destination, err := inputChannel(ctx, s.api, channelID)
	if err != nil {
		return nil, err
	}
	s.destinations[channelID] = destination
	return destination, nil
}

// importDocument extracts the importable document of a message.
func importDocument(message *tg.Message) (ImportDocument, bool) {
	document, ok := messageDocument(message)
	if !ok {
		return ImportDocument{}, false
	}
	result := ImportDocument{
		MessageID: int64(message.ID),
		Date:      time.Unix(int64(message.Date), 0).UTC(),
		MIMEType:  strings.TrimSpace(document.MimeType),
		Size:      document.Size,
		Kind:      "document",
		Caption:   firstLine(message.Message, maxImportCaption),
		document:  &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference},
	}
	for _, attribute := range document.Attributes {
		switch value := attribute.(type) {
		case *tg.DocumentAttributeFilename:
			result.FileName = strings.TrimSpace(value.FileName)
		case *tg.DocumentAttributeVideo:
			if result.Kind == "document" {
				result.Kind = "video"
			}
		case *tg.DocumentAttributeAudio:
			if value.Voice {
				result.Kind = "voice"
			} else {
				result.Kind = "audio"
			}
		case *tg.DocumentAttributeAnimated:
			result.Kind = "animation"
		case *tg.DocumentAttributeSticker:
			result.Kind = "sticker"
		}
	}
	return result, true
}

func firstLine(text string, limit int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	if runes := []rune(line); len(runes) > limit {
		line = strings.TrimSpace(string(runes[:limit]))
	}
	return line
}

var _ DocumentImporter = (*GotdStorage)(nil)
