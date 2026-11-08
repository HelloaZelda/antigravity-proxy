package cache

import (
	"sync"
	"time"
)

// Fallback thought signature when none is found in cache
const DefaultFallbackSignature = "Er4CCrsCAWkUfRO0P+PkJXedge652nfQJ3ZLYDLZyjuphJCnsiiHFtz/a2V1ztDKKjli0gpGckKGb5i6SmN7B2HecXDeubh7McOOZ6xZ28yOEVZ2ryiOG3e43s9YyvRM/dHS2otTkDrV8lFxED7Z1cV1e3cjLpi8AEAPQXmeWCgZ6/ob8Tq3XYwgcZJX49xP078/+Fk/7YB16AJcUEpqIhrtfqXc5tWI2P+gLXvrWYQNoQcchJG9oLaQ/uS/EZtv2j5gRPGHMXqkKew9qYUOWBJ1egXeaNn28Ml2EQtjjTxsh3TpokbKiRG5rdjXmLPjY0NkK8h7Lq0wqrnBVmjINJ3m0JDW9WVfPzgZFLT5ouRA3SX+D+apV3+9i3LA9s5wRY4i/oRVQD878R/prqpAFJ41bPSl9NnALWGvKJJQllm8"

type cacheEntry struct {
	signature string
	expiresAt time.Time
}

// SignatureCache caches thought signatures across conversation turns
type SignatureCache struct {
	mu    sync.RWMutex
	cache map[string]cacheEntry
	ttl   time.Duration
}

// NewSignatureCache creates a new in-memory signature cache
func NewSignatureCache(ttl time.Duration) *SignatureCache {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	c := &SignatureCache{
		cache: make(map[string]cacheEntry),
		ttl:   ttl,
	}
	// Start periodic cleaner
	go c.startCleaner()
	return c
}

// Get retrieves a signature by key (call ID or function name)
func (c *SignatureCache) Get(key string) string {
	if key == "" {
		return DefaultFallbackSignature
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	if entry, ok := c.cache[key]; ok {
		if time.Now().Before(entry.expiresAt) {
			return entry.signature
		}
	}
	return DefaultFallbackSignature
}

// Set stores a signature with TTL
func (c *SignatureCache) Set(key, signature string) {
	if key == "" || signature == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache[key] = cacheEntry{
		signature: signature,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Count returns the active cache size
func (c *SignatureCache) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

func (c *SignatureCache) startCleaner() {
	ticker := time.NewTicker(10 * time.Minute)
	for range ticker.C {
		now := time.Now()
		c.mu.Lock()
		for k, v := range c.cache {
			if now.After(v.expiresAt) {
				delete(c.cache, k)
			}
		}
		c.mu.Unlock()
	}
}
