// Package telegramimport imports documents that already exist in Telegram
// channels into the TelDrive catalog without downloading or uploading them.
package telegramimport

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

// MaxRequestedMessages bounds the message IDs one import may name explicitly,
// counting every message of every range. Channel scans are not bounded.
const MaxRequestedMessages = 10000

// botAPIChannelOffset converts between Bot API chat IDs (-100xxxxxxxxxx) and
// MTProto channel IDs.
const botAPIChannelOffset = 1_000_000_000_000

var ErrInvalidSource = errors.New("invalid Telegram import source")

var usernamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{2,63}$`)

// MessageRange is an inclusive range of message IDs.
type MessageRange struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

func (r MessageRange) count() int64 { return r.To - r.From + 1 }

// Source is one channel to import from: its whole history, explicit
// messages, or both.
type Source struct {
	ChannelID int64          `json:"channel_id,omitempty"`
	Username  string         `json:"username,omitempty"`
	Scan      bool           `json:"scan,omitempty"`
	Messages  []MessageRange `json:"messages,omitempty"`
}

// Ref returns the channel reference for Telegram resolution.
func (s Source) Ref() telegramstore.ChannelRef {
	return telegramstore.ChannelRef{ID: s.ChannelID, Username: s.Username}
}

// Label names the source channel in progress output.
func (s Source) Label() string {
	if s.Username != "" {
		return "@" + s.Username
	}
	return strconv.FormatInt(s.ChannelID, 10)
}

func (s Source) key() string {
	if s.Username != "" {
		return "@" + strings.ToLower(s.Username)
	}
	return strconv.FormatInt(s.ChannelID, 10)
}

// Request is the user's description of what to import.
type Request struct {
	// Channel is a channel ID (with or without the Bot API -100 prefix), an
	// @username, or a t.me link to the channel. Scan and MessageIDs apply to it.
	Channel string
	// Scan imports every document of Channel.
	Scan bool
	// MessageIDs lists message IDs and inclusive ranges of Channel such as
	// "12", "15-20" or "12, 15-20".
	MessageIDs []string
	// Links lists message links such as https://t.me/c/1234567890/45,
	// https://t.me/name/45 or ranges like https://t.me/c/1234567890/45-60.
	Links []string
}

// ParseSources validates req and groups what it names by channel.
func ParseSources(req Request) ([]Source, error) {
	sources := make(map[string]*Source)
	var order []string
	add := func(source Source) {
		key := source.key()
		existing := sources[key]
		if existing == nil {
			order = append(order, key)
			copied := source
			sources[key] = &copied
			return
		}
		existing.Scan = existing.Scan || source.Scan
		existing.Messages = append(existing.Messages, source.Messages...)
	}

	channel := strings.TrimSpace(req.Channel)
	if channel != "" {
		source, err := ParseChannel(channel)
		if err != nil {
			return nil, err
		}
		source.Scan = req.Scan
		for _, value := range req.MessageIDs {
			ranges, err := ParseMessageIDs(value)
			if err != nil {
				return nil, err
			}
			source.Messages = append(source.Messages, ranges...)
		}
		if !source.Scan && len(source.Messages) == 0 {
			return nil, fmt.Errorf("%w: choose a channel scan or list message IDs for %s", ErrInvalidSource, source.Label())
		}
		add(source)
	} else if req.Scan || len(req.MessageIDs) > 0 {
		return nil, fmt.Errorf("%w: a channel is required for a scan or message IDs", ErrInvalidSource)
	}
	for _, link := range req.Links {
		if strings.TrimSpace(link) == "" {
			continue
		}
		source, err := ParseLink(link)
		if err != nil {
			return nil, err
		}
		add(source)
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("%w: nothing to import", ErrInvalidSource)
	}

	result := make([]Source, 0, len(order))
	var requested int64
	for _, key := range order {
		source := *sources[key]
		source.Messages = mergeRanges(source.Messages)
		for _, item := range source.Messages {
			requested += item.count()
		}
		result = append(result, source)
	}
	if requested > MaxRequestedMessages {
		return nil, fmt.Errorf("%w: %d messages requested; at most %d may be listed per import, use a channel scan for more", ErrInvalidSource, requested, MaxRequestedMessages)
	}
	return result, nil
}

// ParseChannel parses a channel ID, Bot API chat ID, @username or t.me link.
func ParseChannel(raw string) (Source, error) {
	value := strings.TrimSpace(raw)
	switch {
	case value == "":
		return Source{}, fmt.Errorf("%w: channel is empty", ErrInvalidSource)
	case isLink(value):
		source, err := parseLink(value, false)
		if err != nil {
			return Source{}, err
		}
		source.Messages = nil
		return source, nil
	case strings.HasPrefix(value, "@"):
		return parseUsername(value[1:])
	}
	if id, err := strconv.ParseInt(value, 10, 64); err == nil {
		return parseChannelID(id, raw)
	}
	return parseUsername(value)
}

// ParseLink parses one message link into its channel and messages.
func ParseLink(raw string) (Source, error) {
	return parseLink(strings.TrimSpace(raw), true)
}

// ParseMessageIDs parses comma- or space-separated message IDs and inclusive
// ranges such as "12, 15-20".
func ParseMessageIDs(raw string) ([]MessageRange, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == ';' })
	ranges := make([]MessageRange, 0, len(fields))
	for _, field := range fields {
		item, err := parseRange(field)
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, item)
	}
	return ranges, nil
}

func parseRange(raw string) (MessageRange, error) {
	from, to, isRange := strings.Cut(strings.TrimSpace(raw), "-")
	start, err := strconv.ParseInt(strings.TrimSpace(from), 10, 64)
	if err != nil || start <= 0 {
		return MessageRange{}, fmt.Errorf("%w: %q is not a message ID or range", ErrInvalidSource, raw)
	}
	end := start
	if isRange {
		end, err = strconv.ParseInt(strings.TrimSpace(to), 10, 64)
		if err != nil || end < start {
			return MessageRange{}, fmt.Errorf("%w: %q is not a message range", ErrInvalidSource, raw)
		}
	}
	if end-start >= MaxRequestedMessages {
		return MessageRange{}, fmt.Errorf("%w: range %q is longer than %d messages; use a channel scan", ErrInvalidSource, raw, MaxRequestedMessages)
	}
	return MessageRange{From: start, To: end}, nil
}

func parseChannelID(id int64, raw string) (Source, error) {
	switch {
	case id > 0:
		return Source{ChannelID: id}, nil
	case id < -botAPIChannelOffset:
		return Source{ChannelID: -id - botAPIChannelOffset}, nil
	default:
		return Source{}, fmt.Errorf("%w: %q is not a channel ID; basic groups are not supported", ErrInvalidSource, raw)
	}
}

func parseUsername(raw string) (Source, error) {
	username := strings.TrimSpace(raw)
	if !usernamePattern.MatchString(username) {
		return Source{}, fmt.Errorf("%w: %q is not a channel ID, @username or t.me link", ErrInvalidSource, raw)
	}
	return Source{Username: username}, nil
}

func isLink(value string) bool {
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "tg://") {
		return true
	}
	lower = strings.TrimPrefix(strings.TrimPrefix(lower, "https://"), "http://")
	lower = strings.TrimPrefix(lower, "www.")
	for _, host := range []string{"t.me/", "telegram.me/", "telegram.dog/"} {
		if strings.HasPrefix(lower, host) {
			return true
		}
	}
	return false
}

// parseLink parses t.me, telegram.me, telegram.dog and tg:// links. With
// requireMessage the link must point at a message or message range.
func parseLink(raw string, requireMessage bool) (Source, error) {
	invalid := func() (Source, error) {
		return Source{}, fmt.Errorf("%w: %q is not a Telegram message link", ErrInvalidSource, raw)
	}
	if !isLink(raw) {
		return invalid()
	}
	value := raw
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return invalid()
	}
	var source Source
	var message string
	if strings.EqualFold(parsed.Scheme, "tg") {
		query := parsed.Query()
		switch strings.ToLower(parsed.Host) {
		case "privatepost":
			id, err := strconv.ParseInt(query.Get("channel"), 10, 64)
			if err != nil {
				return invalid()
			}
			if source, err = parseChannelID(id, raw); err != nil {
				return Source{}, err
			}
		case "resolve":
			if source, err = parseUsername(query.Get("domain")); err != nil {
				return Source{}, err
			}
		default:
			return invalid()
		}
		message = query.Get("post")
	} else {
		segments := strings.FieldsFunc(parsed.Path, func(r rune) bool { return r == '/' })
		if len(segments) > 0 && segments[0] == "s" {
			// t.me/s/<username>/<message> is the web preview of a public channel.
			segments = segments[1:]
		}
		if len(segments) == 0 {
			return invalid()
		}
		if segments[0] == "c" {
			if len(segments) < 2 {
				return invalid()
			}
			id, err := strconv.ParseInt(segments[1], 10, 64)
			if err != nil || id <= 0 {
				return invalid()
			}
			source = Source{ChannelID: id}
			segments = segments[2:]
		} else {
			if source, err = parseUsername(segments[0]); err != nil {
				return Source{}, err
			}
			segments = segments[1:]
		}
		if len(segments) > 0 {
			// Forum links carry a topic before the message: keep the last segment.
			message = segments[len(segments)-1]
		}
	}
	if message == "" {
		if requireMessage {
			return Source{}, fmt.Errorf("%w: %q does not point at a message; use the channel field to scan a channel", ErrInvalidSource, raw)
		}
		return source, nil
	}
	item, err := parseRange(message)
	if err != nil {
		return Source{}, err
	}
	source.Messages = []MessageRange{item}
	return source, nil
}

// mergeRanges sorts ranges and merges overlapping and adjacent ones.
func mergeRanges(ranges []MessageRange) []MessageRange {
	if len(ranges) == 0 {
		return nil
	}
	sorted := slices.Clone(ranges)
	slices.SortFunc(sorted, func(a, b MessageRange) int { return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To)) })
	merged := sorted[:1]
	for _, item := range sorted[1:] {
		last := &merged[len(merged)-1]
		if item.From <= last.To+1 {
			last.To = max(last.To, item.To)
			continue
		}
		merged = append(merged, item)
	}
	return merged
}

// expand lists the message IDs of ranges in batches of at most size IDs.
func expand(ranges []MessageRange, size int, fn func([]int64) error) error {
	batch := make([]int64, 0, size)
	for _, item := range ranges {
		for id := item.From; id <= item.To; id++ {
			batch = append(batch, id)
			if len(batch) == size {
				if err := fn(slices.Clone(batch)); err != nil {
					return err
				}
				batch = batch[:0]
			}
		}
	}
	if len(batch) > 0 {
		return fn(batch)
	}
	return nil
}
