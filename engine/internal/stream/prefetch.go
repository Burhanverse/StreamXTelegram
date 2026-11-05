package stream

import (
	"fmt"
	"sync"
)

// PrefetchCache is a thread-safe in-memory ring buffer for prefetched MTProto audio chunks.
// It eliminates cold-chunk network latency during sequential playback.
type PrefetchCache struct {
	mu       sync.RWMutex
	capacity int
	order    []string
	chunks   map[string][]byte
}

// NewPrefetchCache creates a prefetch cache with an entry capacity limit.
func NewPrefetchCache(capacity int) *PrefetchCache {
	if capacity <= 0 {
		capacity = 16 // 16 entries * ~512 KB = ~8 MB memory footprint
	}
	return &PrefetchCache{
		capacity: capacity,
		order:    make([]string, 0, capacity),
		chunks:   make(map[string][]byte),
	}
}

func cacheKey(trackID string, offset int64, limit int) string {
	return fmt.Sprintf("%s:%d:%d", trackID, offset, limit)
}

// Get retrieves a prefetched chunk from the cache if available.
// It returns an immutable slice directly (zero-copy) to avoid allocation overhead on high-frequency reads.
func (c *PrefetchCache) Get(trackID string, offset int64, limit int) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	data, ok := c.chunks[cacheKey(trackID, offset, limit)]
	if !ok {
		return nil, false
	}
	return data, true
}

// Has checks if a chunk is currently in cache without retrieving the data.
func (c *PrefetchCache) Has(trackID string, offset int64, limit int) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.chunks[cacheKey(trackID, offset, limit)]
	return ok
}

// Put adds a prefetched chunk into the cache, evicting the oldest entry when at capacity.
func (c *PrefetchCache) Put(trackID string, offset int64, limit int, data []byte) {
	if len(data) == 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey(trackID, offset, limit)
	if _, exists := c.chunks[key]; exists {
		c.chunks[key] = data
		return
	}

	if len(c.order) >= c.capacity {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.chunks, oldest)
	}

	c.order = append(c.order, key)
	c.chunks[key] = data
}

// Clear flushes all cached chunks from memory.
func (c *PrefetchCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order = c.order[:0]
	c.chunks = make(map[string][]byte)
}
