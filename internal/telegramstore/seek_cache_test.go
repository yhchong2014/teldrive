package telegramstore

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func TestSeekChunkCacheEvictionIsolationAndCopies(t *testing.T) {
	cache := newSeekChunkCache(6, time.Minute)
	first := seekChunkKey{userID: 1, documentID: 10, offset: 0, limit: 3}
	second := seekChunkKey{userID: 1, documentID: 10, offset: 3, limit: 3}
	third := seekChunkKey{userID: 1, documentID: 10, offset: 6, limit: 3}
	input := []byte("abc")
	cache.put(first, input)
	input[0] = 'x'
	if got, ok := cache.get(first); !ok || !bytes.Equal(got, []byte("abc")) {
		t.Fatalf("first = %q, %v", got, ok)
	}
	otherUser := first
	otherUser.userID = 2
	if _, ok := cache.get(otherUser); ok {
		t.Fatal("cache leaked across users")
	}
	otherDocument := first
	otherDocument.documentID++
	if _, ok := cache.get(otherDocument); ok {
		t.Fatal("cache reused after document changed")
	}
	cache.put(second, []byte("def"))
	cache.get(first) // mark the first entry as recently used
	cache.put(third, []byte("ghi"))
	if _, ok := cache.get(second); ok {
		t.Fatal("least recently used entry was not evicted")
	}
	if _, ok := cache.get(first); !ok {
		t.Fatal("recent entry was evicted")
	}
	if cache.size > cache.maxBytes {
		t.Fatalf("cache size = %d", cache.size)
	}
}

func TestSeekChunkCacheExpiryAndInvalidKeys(t *testing.T) {
	cache := newSeekChunkCache(10, time.Minute)
	key := seekChunkKey{userID: 1, documentID: 2}
	cache.put(key, []byte("abc"))
	cache.mu.Lock()
	cache.entries[key].Value.(*seekChunkEntry).expires = time.Now().Add(-time.Second)
	cache.mu.Unlock()
	if _, ok := cache.get(key); ok {
		t.Fatal("expired entry was returned")
	}
	cache.put(seekChunkKey{}, []byte("abc"))
	cache.put(key, make([]byte, 11))
	if cache.size != 0 {
		t.Fatalf("invalid entries were cached: %d", cache.size)
	}
}

func TestSeekChunkCacheReusesDownloadedAlignedChunk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	invoker := &fallbackDownloadInvoker{}
	client := rangeClient{api: tg.NewClient(invoker), clientID: 1}
	reader := newTelegramRangeReader(ctx, cancel, 3, 1, nil)
	reader.seekCache = newSeekChunkCache(2*telegramReadAlign, time.Minute)
	location := &tg.InputDocumentFileLocation{ID: 10, AccessHash: 20}
	request := RangeRequest{UserID: 7, ChannelID: 55, MessageID: 5, Offset: telegramReadAlign, Length: telegramReadAlign}
	for range 2 {
		if err := reader.fill(ctx, []rangeClient{client}, request, location, telegramReadAlign); err != nil {
			t.Fatal(err)
		}
		<-reader.buffers
	}
	if got := invoker.downloads.Load(); got != 1 {
		t.Fatalf("Telegram fetches = %d, want 1", got)
	}
}
