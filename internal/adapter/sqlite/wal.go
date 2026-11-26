package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

const (
	walCheckpointInterval = 250 * time.Millisecond
	walCheckpointWarn     = 30 * time.Second
	walLargeLogPages      = 4096
)

func disableWALAutoCheckpoint(conn *sqlite3.SQLiteConn) error {
	// The threshold is per connection, including replacement writers.
	_, err := conn.Exec("PRAGMA wal_autocheckpoint = 0", nil)
	return err
}

// startWALCheckpoint moves checkpoint I/O to a dedicated connection. PASSIVE
// checkpoints can make progress alongside requests and retry on the next tick.
func startWALCheckpoint(ctx context.Context, dsn string) (func() error, error) {
	path, query, _ := strings.Cut(dsn, "?")
	options, err := url.ParseQuery(query)
	if err != nil {
		return nil, err
	}
	options.Del("_query_only")
	// A busy reader must not stall other requests during the fallback.
	options.Set("_busy_timeout", "0")
	pool := sql.OpenDB(&sqliteConnector{dsn: path + "?" + options.Encode()})
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	if err := pool.PingContext(ctx); err != nil {
		return nil, errors.Join(err, pool.Close())
	}

	lifetime, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(walCheckpointInterval)
		defer ticker.Stop()
		var previousErr error
		var stalledSince, warnedAt time.Time
		for {
			select {
			case <-lifetime.Done():
				return
			case now := <-ticker.C:
				var busy, pages, checkpointed int
				err := pool.QueryRowContext(lifetime, "PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &pages, &checkpointed)
				if err != nil && previousErr == nil && lifetime.Err() == nil {
					slog.Warn("SQLite WAL checkpoint failed", "error", err)
				}
				previousErr = err
				if err != nil || pages < walLargeLogPages {
					stalledSince = time.Time{}
					continue
				}
				if busy == 0 && checkpointed >= pages {
					err = pool.QueryRowContext(lifetime, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &pages, &checkpointed)
					if err == nil && busy == 0 {
						stalledSince = time.Time{}
						continue
					}
				}
				if stalledSince.IsZero() {
					stalledSince = now
				}
				if now.Sub(stalledSince) >= walCheckpointWarn && now.Sub(warnedAt) >= walCheckpointWarn {
					slog.Warn("SQLite WAL could not be reset", "pages", pages, "checkpointed", checkpointed, "busy", busy, "error", err)
					warnedAt = now
				}
			}
		}
	}()
	return func() error {
		cancel()
		<-stopped
		return pool.Close()
	}, nil
}
