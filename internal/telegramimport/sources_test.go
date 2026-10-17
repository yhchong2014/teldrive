package telegramimport

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"

	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

func TestParseLinkFormats(t *testing.T) {
	tests := []struct {
		link string
		want Source
	}{
		{"https://t.me/c/1234567890/45", Source{ChannelID: 1234567890, Messages: []MessageRange{{45, 45}}}},
		{"t.me/c/1234567890/45-60", Source{ChannelID: 1234567890, Messages: []MessageRange{{45, 60}}}},
		{"https://t.me/c/1234567890/7/45?single", Source{ChannelID: 1234567890, Messages: []MessageRange{{45, 45}}}},
		{"https://t.me/movies_hd/88", Source{Username: "movies_hd", Messages: []MessageRange{{88, 88}}}},
		{"https://t.me/s/movies_hd/88", Source{Username: "movies_hd", Messages: []MessageRange{{88, 88}}}},
		{"https://www.telegram.me/movies_hd/88?comment=3", Source{Username: "movies_hd", Messages: []MessageRange{{88, 88}}}},
		{"http://telegram.dog/movies_hd/1-3", Source{Username: "movies_hd", Messages: []MessageRange{{1, 3}}}},
		{"tg://privatepost?channel=1234567890&post=45", Source{ChannelID: 1234567890, Messages: []MessageRange{{45, 45}}}},
		{"tg://resolve?domain=movies_hd&post=9", Source{Username: "movies_hd", Messages: []MessageRange{{9, 9}}}},
	}
	for _, test := range tests {
		got, err := ParseLink(test.link)
		if err != nil {
			t.Fatalf("ParseLink(%q) error = %v", test.link, err)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("ParseLink(%q) = %+v, want %+v", test.link, got, test.want)
		}
	}
}

func TestParseLinkRejectsInvalidLinks(t *testing.T) {
	for _, link := range []string{
		"https://example.com/c/1/2",
		"https://t.me/c/1234567890",
		"https://t.me/movies_hd",
		"https://t.me/c/abc/5",
		"https://t.me/c/1234567890/0",
		"https://t.me/c/1234567890/9-3",
		"https://t.me/c/1234567890/1-20000",
		"tg://user?id=5",
		"https://t.me/+AbCdEf",
	} {
		if _, err := ParseLink(link); !errors.Is(err, ErrInvalidSource) {
			t.Fatalf("ParseLink(%q) error = %v, want ErrInvalidSource", link, err)
		}
	}
}

func TestParseChannelForms(t *testing.T) {
	tests := map[string]Source{
		"1234567890":                     {ChannelID: 1234567890},
		"-1001234567890":                 {ChannelID: 1234567890},
		"@movies_hd":                     {Username: "movies_hd"},
		"movies_hd":                      {Username: "movies_hd"},
		"https://t.me/c/1234567890/45":   {ChannelID: 1234567890},
		"https://t.me/movies_hd":         {Username: "movies_hd"},
		" https://t.me/c/1234567890/ ":   {ChannelID: 1234567890},
		"tg://resolve?domain=movies_hd":  {Username: "movies_hd"},
		"tg://privatepost?channel=12345": {ChannelID: 12345},
	}
	for raw, want := range tests {
		got, err := ParseChannel(raw)
		if err != nil {
			t.Fatalf("ParseChannel(%q) error = %v", raw, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ParseChannel(%q) = %+v, want %+v", raw, got, want)
		}
	}
	for _, raw := range []string{"", "-12345", "@a", "not a channel", "0"} {
		if _, err := ParseChannel(raw); !errors.Is(err, ErrInvalidSource) {
			t.Fatalf("ParseChannel(%q) error = %v, want ErrInvalidSource", raw, err)
		}
	}
}

func TestParseSourcesGroupsByChannelAndMergesRanges(t *testing.T) {
	sources, err := ParseSources(Request{
		Channel:    "-1001234567890",
		Scan:       true,
		MessageIDs: []string{"12, 15-20", "18-25 30"},
		Links: []string{
			"https://t.me/c/1234567890/26",
			"https://t.me/Movies_HD/5",
			"https://t.me/movies_hd/6-7",
			"  ",
		},
	})
	if err != nil {
		t.Fatalf("ParseSources() error = %v", err)
	}
	want := []Source{
		{ChannelID: 1234567890, Scan: true, Messages: []MessageRange{{12, 12}, {15, 26}, {30, 30}}},
		{Username: "Movies_HD", Messages: []MessageRange{{5, 7}}},
	}
	if !reflect.DeepEqual(sources, want) {
		t.Fatalf("ParseSources() = %+v, want %+v", sources, want)
	}
}

func TestParseSourcesValidation(t *testing.T) {
	tests := []struct {
		name    string
		request Request
		message string
	}{
		{"nothing", Request{}, "nothing to import"},
		{"channel without work", Request{Channel: "123"}, "choose a channel scan"},
		{"scan without channel", Request{Scan: true}, "a channel is required"},
		{"ids without channel", Request{MessageIDs: []string{"5"}}, "a channel is required"},
		{"bad id", Request{Channel: "123", MessageIDs: []string{"5x"}}, "not a message ID"},
		{"too many", Request{Channel: "123", MessageIDs: []string{"1-9999", "20000-20002"}}, "at most"},
	}
	for _, test := range tests {
		_, err := ParseSources(test.request)
		if !errors.Is(err, ErrInvalidSource) || !strings.Contains(err.Error(), test.message) {
			t.Fatalf("%s: error = %v, want %q", test.name, err, test.message)
		}
	}
}

func TestExpandBatchesWithoutSharingSlices(t *testing.T) {
	var batches [][]int64
	err := expand([]MessageRange{{1, 3}, {10, 11}}, 2, func(ids []int64) error {
		batches = append(batches, ids)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]int64{{1, 2}, {3, 10}, {11}}
	if !reflect.DeepEqual(batches, want) {
		t.Fatalf("batches = %v, want %v", batches, want)
	}
}

func TestDocumentName(t *testing.T) {
	date := time.Unix(0, 0)
	tests := []struct {
		document telegramstore.ImportDocument
		want     string
	}{
		{telegramstore.ImportDocument{MessageID: 1, FileName: "Movie.2024.1080p.mkv", MIMEType: "video/x-matroska", Kind: "video"}, "Movie.2024.1080p.mkv"},
		{telegramstore.ImportDocument{MessageID: 2, FileName: "a/b\\c.txt"}, "a_b_c.txt"},
		{telegramstore.ImportDocument{MessageID: 3, Caption: "The  Movie (2024)", MIMEType: "video/mp4", Kind: "video"}, "The Movie (2024).mp4"},
		{telegramstore.ImportDocument{MessageID: 4, Caption: "clip.mp4", MIMEType: "video/mp4", Kind: "video"}, "clip.mp4"},
		{telegramstore.ImportDocument{MessageID: 5, MIMEType: "video/mp4", Kind: "video"}, "video_5.mp4"},
		{telegramstore.ImportDocument{MessageID: 6, MIMEType: "audio/ogg; codecs=opus", Kind: "voice"}, "voice_6.ogg"},
		{telegramstore.ImportDocument{MessageID: 7}, "document_7"},
		{telegramstore.ImportDocument{MessageID: 8, FileName: " .. ", Caption: "\x00\x01", MIMEType: "application/x-unknown-type"}, "document_8"},
	}
	for _, test := range tests {
		test.document.Date = date
		if got := DocumentName(test.document); got != test.want {
			t.Fatalf("DocumentName(%+v) = %q, want %q", test.document, got, test.want)
		}
	}
	long := DocumentName(telegramstore.ImportDocument{MessageID: 9, Caption: strings.Repeat("é", 300), MIMEType: "video/mp4"})
	if runes := []rune(long); len(runes) != maxNameRunes+len(".mp4") {
		t.Fatalf("long caption name has %d runes", len(runes))
	}
}

func TestParseMode(t *testing.T) {
	for raw, want := range map[string]Mode{"": ModeAuto, "AUTO": ModeAuto, "reference": ModeReference, " copy ": ModeCopy} {
		if got, err := ParseMode(raw); err != nil || got != want {
			t.Fatalf("ParseMode(%q) = %q, %v", raw, got, err)
		}
	}
	if _, err := ParseMode("move"); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("ParseMode(move) error = %v", err)
	}
}

func TestWithFloodRetrySitsOutFloodWaits(t *testing.T) {
	var slept []time.Duration
	sleep := func(_ context.Context, d time.Duration) bool {
		slept = append(slept, d)
		return true
	}
	calls := 0
	result, err := withFloodRetry(context.Background(), sleep, func() (int, error) {
		calls++
		if calls < 3 {
			return 0, tgerr.New(420, "FLOOD_WAIT_90")
		}
		return 42, nil
	})
	if err != nil || result != 42 || calls != 3 {
		t.Fatalf("withFloodRetry() = %d, %v after %d calls", result, err, calls)
	}
	if len(slept) != 2 || slept[0] != 91*time.Second {
		t.Fatalf("slept %v, want two waits of 91s", slept)
	}

	calls = 0
	_, err = withFloodRetry(context.Background(), sleep, func() (int, error) {
		calls++
		return 0, tgerr.New(420, "FLOOD_WAIT_7200")
	})
	if calls != 1 || !tgerr.Is(err, "FLOOD_WAIT") {
		t.Fatalf("long flood wait: %d calls, error %v; want no retry", calls, err)
	}

	calls = 0
	_, err = withFloodRetry(context.Background(), sleep, func() (int, error) {
		calls++
		return 0, errors.New("CHANNEL_PRIVATE")
	})
	if calls != 1 || err == nil {
		t.Fatalf("other error: %d calls, error %v", calls, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = withFloodRetry(ctx, func(context.Context, time.Duration) bool { return false }, func() (int, error) {
		return 0, tgerr.New(420, "FLOOD_WAIT_5")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait error = %v", err)
	}
}
