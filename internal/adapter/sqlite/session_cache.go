package sqlite

import (
	"container/list"
	"sync"
	"time"

	"github.com/OXeu/shinona/internal/auth"
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

type sessionEntry struct {
	key string
	cachedSession
}

type sessionCache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	recent     list.List
	generation uint64
	owners     map[string]int
	keys       map[string]int
}

func (c *sessionCache) get(key string, now time.Time) (cachedSession, uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[key]; element != nil {
		entry := element.Value.(sessionEntry)
		if now.Before(entry.expiresAt) {
			c.recent.MoveToFront(element)
			return entry.cachedSession, c.generation, true
		}
		c.remove(element)
	}
	return cachedSession{}, c.generation, false
}

func (c *sessionCache) put(key string, generation uint64, entry cachedSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation || c.keys[key] > 0 || c.owners[entry.user.ID] > 0 {
		return
	}
	if element := c.entries[key]; element != nil {
		c.remove(element)
	}
	if c.recent.Len() >= sessionCacheSize {
		c.remove(c.recent.Back())
	}
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	c.entries[key] = c.recent.PushFront(sessionEntry{key, entry})
}

func (c *sessionCache) remove(element *list.Element) {
	delete(c.entries, element.Value.(sessionEntry).key)
	c.recent.Remove(element)
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
		if element := c.entries[key]; element != nil {
			c.remove(element)
		}
	} else {
		if c.owners == nil {
			c.owners = make(map[string]int)
			pending = c.owners
		}
		for _, element := range c.entries {
			if element.Value.(sessionEntry).user.ID == owner {
				c.remove(element)
			}
		}
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
