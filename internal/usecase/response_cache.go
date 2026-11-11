package usecase

import (
	"sync"
	"time"
)

// ResponseMapCache remembers wire maps by upstream response ID so
// GET /v1/responses/{id} can decode tool names exactly. Entries expire after
// ttl; the table is bounded (random eviction past max) so a busy router
// cannot grow it without limit. A miss degrades to raw passthrough.
type ResponseMapCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	ttl     time.Duration
	max     int
}

type cacheEntry struct {
	wireMap map[string]string
	expires time.Time
}

// NewResponseMapCache builds a cache with the given ttl and max entries.
func NewResponseMapCache(ttl time.Duration, max int) *ResponseMapCache {
	if max < 1 {
		max = 1
	}
	return &ResponseMapCache{entries: make(map[string]cacheEntry), ttl: ttl, max: max}
}

// Store remembers wireMap under id. Empty ids and tool-less maps are skipped:
// without tools there is nothing to decode.
func (c *ResponseMapCache) Store(id string, wireMap map[string]string) {
	if id == "" || len(wireMap) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		c.sweepLocked(time.Now())
	}
	for len(c.entries) >= c.max {
		for key := range c.entries {
			delete(c.entries, key)
			break
		}
	}
	c.entries[id] = cacheEntry{wireMap: wireMap, expires: time.Now().Add(c.ttl)}
}

// Get returns the wire map for id, or false when unknown or expired.
func (c *ResponseMapCache) Get(id string) (map[string]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id]
	if !ok || time.Now().After(entry.expires) {
		delete(c.entries, id)
		return nil, false
	}
	return entry.wireMap, true
}

// Len returns the entry count, for tests.
func (c *ResponseMapCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *ResponseMapCache) sweepLocked(now time.Time) {
	for key, entry := range c.entries {
		if now.After(entry.expires) {
			delete(c.entries, key)
		}
	}
}
