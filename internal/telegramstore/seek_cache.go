package telegramstore

import (
	"container/list"
	"sync"
	"time"
)

// seekChunkCache holds only short-lived Telegram file chunks in process memory.
// Keys include the catalog owner and document identity to avoid cross-user and
// edited-document reuse. Nothing is written to disk or the shared cache.
type seekChunkKey struct {
	userID, channelID, messageID int64
	documentID, accessHash       int64
	offset                       int64
	limit                        int
}

type seekChunkEntry struct {
	key     seekChunkKey
	data    []byte
	expires time.Time
}

type seekChunkCache struct {
	mu       sync.Mutex
	entries  map[seekChunkKey]*list.Element
	lru      list.List
	size     int
	maxBytes int
	ttl      time.Duration
}

func newSeekChunkCache(maxBytes int, ttl time.Duration) *seekChunkCache {
	return &seekChunkCache{entries: make(map[seekChunkKey]*list.Element), maxBytes: maxBytes, ttl: ttl}
}

func (c *seekChunkCache) get(key seekChunkKey) ([]byte, bool) {
	if c == nil || key.userID <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	item := c.entries[key]
	if item == nil {
		return nil, false
	}
	entry := item.Value.(*seekChunkEntry)
	if !time.Now().Before(entry.expires) {
		c.remove(item)
		return nil, false
	}
	c.lru.MoveToFront(item)
	return entry.data, true // callers treat cached bytes as immutable
}

func (c *seekChunkCache) put(key seekChunkKey, data []byte) {
	if c == nil || key.userID <= 0 || key.documentID == 0 || len(data) == 0 || len(data) > c.maxBytes || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.entries[key]; old != nil {
		c.remove(old)
	}
	for c.size+len(data) > c.maxBytes {
		c.remove(c.lru.Back())
	}
	entry := &seekChunkEntry{key: key, data: append([]byte(nil), data...), expires: time.Now().Add(c.ttl)}
	c.entries[key] = c.lru.PushFront(entry)
	c.size += len(entry.data)
}

// remove requires c.mu to be held.
func (c *seekChunkCache) remove(item *list.Element) {
	entry := item.Value.(*seekChunkEntry)
	delete(c.entries, entry.key)
	c.size -= len(entry.data)
	c.lru.Remove(item)
}
