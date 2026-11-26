package sqlite

import (
	"sync"
	"time"

	"github.com/OXeu/shinona/internal/auth"
)

const (
	sessionCacheSize = 4096
	// Commits in this process invalidate the affected entries immediately.
	// External writers can leave an authentication result valid for this long.
	sessionCacheTTL = 2 * time.Second
)

type cachedSession struct {
	user      auth.User
	expiresAt time.Time
}

type sessionCache struct {
	mu         sync.RWMutex
	entries    map[string]cachedSession
	generation uint64
}

func (c *sessionCache) get(key string, now time.Time) (cachedSession, uint64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, found := c.entries[key]
	return entry, c.generation, found && now.Before(entry.expiresAt)
}

func (c *sessionCache) put(key string, generation uint64, entry cachedSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// An invalidation can race a lookup that observed the pre-commit state.
	if generation != c.generation {
		return
	}
	if _, cached := c.entries[key]; !cached && len(c.entries) >= sessionCacheSize {
		for oldest := range c.entries {
			delete(c.entries, oldest)
			break
		}
	}
	if c.entries == nil {
		c.entries = make(map[string]cachedSession)
	}
	c.entries[key] = entry
}

func (c *sessionCache) invalidateKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	delete(c.entries, key)
}

func (c *sessionCache) invalidateOwner(owner string, err error) {
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	for key, entry := range c.entries {
		if entry.user.ID == owner {
			delete(c.entries, key)
		}
	}
}
