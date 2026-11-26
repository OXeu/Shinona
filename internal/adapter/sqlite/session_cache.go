package sqlite

import (
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/auth"
)

const (
	sessionCacheSize   = 4096
	sessionRevisionSQL = "SELECT revision FROM session_auth_revision WHERE id = 1"
)

type cachedSession struct {
	user      auth.User
	expiresAt time.Time
	revision  int64
}

type sessionCache struct {
	mu       sync.RWMutex
	entries  map[string]cachedSession
	revision int64
}

func (c *sessionCache) get(key string, now time.Time) (cachedSession, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, found := c.entries[key]
	return entry, found && now.Before(entry.expiresAt)
}

func (c *sessionCache) put(key string, entry cachedSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A concurrent lookup may have taken a snapshot before the latest change.
	if entry.revision < c.revision {
		return
	}
	if entry.revision > c.revision || len(c.entries) >= sessionCacheSize {
		clear(c.entries)
		c.revision = entry.revision
	}
	if c.entries == nil {
		c.entries = make(map[string]cachedSession)
	}
	c.entries[key] = entry
}

// SQLite advances the revision in the same transaction as account and session
// changes, including writes from another process.
func initSessionCache(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`CREATE TABLE IF NOT EXISTS session_auth_revision (
			id INTEGER PRIMARY KEY CHECK (id = 1), revision INTEGER NOT NULL
		)`).Error; err != nil {
			return err
		}
		if err := tx.Exec("INSERT OR IGNORE INTO session_auth_revision (id, revision) VALUES (1, 0)").Error; err != nil {
			return err
		}
		for _, table := range []string{"users", "login_sessions"} {
			for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
				query := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS session_auth_%s_%s
					AFTER %s ON %s BEGIN
						UPDATE session_auth_revision SET revision = revision + 1 WHERE id = 1;
					END`, table, op, op, table)
				if err := tx.Exec(query).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}
