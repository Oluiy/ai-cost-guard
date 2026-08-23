// Package cache provides semantic response caching keyed by prompt+model hash.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Cache stores raw response bytes keyed by a request fingerprint.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// Key builds a stable cache key from a model name and the request body's
// significant fields (messages, temperature, etc). Any JSON-serializable
// value works; callers typically pass the parsed request payload.
func Key(model string, payload any) string {
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte{0})
	if b, err := json.Marshal(payload); err == nil {
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// memoryEntry is a single cached value with expiry.
type memoryEntry struct {
	value   []byte
	expires time.Time
}

// MemoryCache is an in-process TTL cache. Safe for concurrent use.
type MemoryCache struct {
	mu      sync.RWMutex
	entries map[string]memoryEntry
	closed  chan struct{}
}

// NewMemoryCache creates an empty in-memory cache and starts a background
// janitor that periodically evicts expired entries.
func NewMemoryCache() *MemoryCache {
	c := &MemoryCache{entries: make(map[string]memoryEntry), closed: make(chan struct{})}
	go c.janitor()
	return c
}

func (c *MemoryCache) janitor() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-ticker.C:
			now := time.Now()
			c.mu.Lock()
			for k, e := range c.entries {
				if now.After(e.expires) {
					delete(c.entries, k)
				}
			}
			c.mu.Unlock()
		}
	}
}

// Close stops the background janitor goroutine. Safe to call once, cache now expired.
func (c *MemoryCache) Close() {
	close(c.closed)
}

func (c *MemoryCache) Get(_ context.Context, key string) ([]byte, bool) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.value, true
}

func (c *MemoryCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	c.entries[key] = memoryEntry{value: value, expires: time.Now().Add(ttl)}
	c.mu.Unlock()
	return nil
}
