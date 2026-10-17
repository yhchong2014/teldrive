package importer

import "testing"

func TestChannelMessageLink(t *testing.T) {
	t.Parallel()
	if got := ChannelMessageLink(1234567890, 42); got != "https://t.me/c/1234567890/42" {
		t.Fatalf("unexpected link %q", got)
	}
}
