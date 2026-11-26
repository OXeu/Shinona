package sqlite

import (
	"sync"
	"time"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/responsecache"
)

const (
	sessionCacheSize = 4096
	// External writers can leave an authentication result valid for this long.
	sessionCacheTTL = 2 * time.Second
)

type cachedSession struct {
	user      auth.User
	expiresAt time.Time
}

type sessionCache struct {
	mu         sync.Mutex
	entries    responsecache.Store[string, cachedSession]
	generation uint64
	owners     map[string]int
	keys       map[string]int
}

func newSessionCache() sessionCache {
	return sessionCache{entries: responsecache.NewStore[string, cachedSession](sessionCacheSize, sessionCacheSize)}
}

func (c *sessionCache) get(key string, now time.Time) (cachedSession, uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries.Get(key); ok {
		if now.Before(entry.expiresAt) {
			return entry, c.generation, true
		}
		c.entries.Delete(key)
	}
	return cachedSession{}, c.generation, false
}

func (c *sessionCache) put(key string, generation uint64, entry cachedSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation || c.keys[key] > 0 || c.owners[entry.user.ID] > 0 {
		return
	}
	c.entries.Put(key, entry, 1, nil)
}

// Remove cached authorization before the transaction and block refills until
// it finishes. A database lookup during the write sees the current snapshot.
func (c *sessionCache) change(key, owner string) func() {
	c.mu.Lock()
	c.generation++
	pending, id := c.owners, owner
	if owner == "" {
		if c.keys == nil {
			c.keys = make(map[string]int)
		}
		pending, id = c.keys, key
		c.entries.Delete(key)
	} else {
		if c.owners == nil {
			c.owners = make(map[string]int)
			pending = c.owners
		}
		c.entries.DeleteWhere(func(entry cachedSession) bool { return entry.user.ID == owner })
	}
	pending[id]++
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.generation++
		pending[id]--
		if pending[id] == 0 {
			delete(pending, id)
		}
	}
}
