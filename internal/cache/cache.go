package cache

import (
	"log"
	"sync"
	"time"
)

// Debug controls whether cache operations are logged
var Debug bool

// Entry represents a cached item
type Entry struct {
	Value     interface{}
	ExpiresAt time.Time
}

// Cache is a simple TTL-based cache
type Cache struct {
	mu      sync.RWMutex
	entries map[string]Entry
	ttl     time.Duration
}

// New creates a new cache with the given TTL
func New(ttl time.Duration) *Cache {
	c := &Cache{
		entries: make(map[string]Entry),
		ttl:     ttl,
	}

	// Start cleanup goroutine
	go c.cleanup()

	return c
}

// Get retrieves a value from the cache
func (c *Cache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[key]
	if !ok {
		if Debug {
			log.Printf("[cache] MISS %s (not found)", key)
		}
		return nil, false
	}

	if time.Now().After(entry.ExpiresAt) {
		if Debug {
			log.Printf("[cache] MISS %s (expired)", key)
		}
		return nil, false
	}

	if Debug {
		log.Printf("[cache] HIT  %s", key)
	}
	return entry.Value, true
}

// Set stores a value in the cache
func (c *Cache) Set(key string, value interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if Debug {
		log.Printf("[cache] SET  %s (ttl: %s)", key, c.ttl)
	}
	c.entries[key] = Entry{
		Value:     value,
		ExpiresAt: time.Now().Add(c.ttl),
	}
}

// SetWithTTL stores a value with a custom TTL
func (c *Cache) SetWithTTL(key string, value interface{}, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = Entry{
		Value:     value,
		ExpiresAt: time.Now().Add(ttl),
	}
}

// Delete removes a value from the cache
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, key)
}

// Clear removes all entries from the cache
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[string]Entry)
}

// cleanup periodically removes expired entries
func (c *Cache) cleanup() {
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()

	for range ticker.C {
		c.mu.Lock()
		now := time.Now()
		for key, entry := range c.entries {
			if now.After(entry.ExpiresAt) {
				delete(c.entries, key)
			}
		}
		c.mu.Unlock()
	}
}
