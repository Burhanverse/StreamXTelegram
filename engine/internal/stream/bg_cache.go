package stream

import (
	"sync"
)

type bgCacheEntry struct {
	trackID string
	offset  int64
	data    []byte
}

// BgChunkCache is a small bounded LRU cache specifically for background tasks
// (such as FLAC header scanning and embedded picture streaming).
// It prevents repeated downloads of the same 1 MB block without polluting the playback prefetchCache.
type BgChunkCache struct {
	mu       sync.Mutex
	capacity int
	entries  []*bgCacheEntry
}

// NewBgChunkCache creates a background chunk cache with an entry capacity (default 4 x 1 MB).
func NewBgChunkCache(capacity int) *BgChunkCache {
	if capacity <= 0 {
		capacity = 4 // 4 x 1 MB = 4 MB max memory
	}
	return &BgChunkCache{
		capacity: capacity,
		entries:  make([]*bgCacheEntry, 0, capacity),
	}
}

// GetRange finds a cached entry for trackID that covers offset.
// If found, returns the chunk slice starting at offset and true.
func (c *BgChunkCache) GetRange(trackID string, offset int64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, e := range c.entries {
		if e.trackID == trackID && offset >= e.offset && offset < e.offset+int64(len(e.data)) {
			// Move to end (most recently used)
			if i < len(c.entries)-1 {
				c.entries = append(append(c.entries[:i], c.entries[i+1:]...), e)
			}
			rel := offset - e.offset
			return e.data[rel:], true
		}
	}
	return nil, false
}

// Get retrieves a chunk if present at the exact offset, moving it to the most-recently-used position.
func (c *BgChunkCache) Get(trackID string, offset int64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, e := range c.entries {
		if e.trackID == trackID && e.offset == offset {
			// Move to end (most recently used)
			if i < len(c.entries)-1 {
				c.entries = append(append(c.entries[:i], c.entries[i+1:]...), e)
			}
			return e.data, true
		}
	}
	return nil, false
}

// Put inserts or updates a chunk in the cache, evicting the oldest entry when at capacity.
func (c *BgChunkCache) Put(trackID string, offset int64, data []byte) {
	if len(data) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, e := range c.entries {
		if e.trackID == trackID && e.offset == offset {
			e.data = data
			if i < len(c.entries)-1 {
				c.entries = append(append(c.entries[:i], c.entries[i+1:]...), e)
			}
			return
		}
	}

	if len(c.entries) >= c.capacity {
		// Evict oldest at index 0
		c.entries = c.entries[1:]
	}

	c.entries = append(c.entries, &bgCacheEntry{
		trackID: trackID,
		offset:  offset,
		data:    data,
	})
}

// Clear flushes all entries from the background cache.
func (c *BgChunkCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = c.entries[:0]
}

