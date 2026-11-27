package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

const (
	walCheckpointInterval = 250 * time.Millisecond
	walCheckpointWarn     = 30 * time.Second
	walLargeLogPages      = 4096
	walHeaderBytes        = 32
	walFrameHeaderBytes   = 24
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
	// Read SQLite's resolved filename so that URI escaping and relative paths
	// are handled the same way as in the writer pool.
	var index int
	var schema, filename string
	if err := pool.QueryRowContext(ctx, "PRAGMA database_list").Scan(&index, &schema, &filename); err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	var pageSize int64
	if err := pool.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	logPath := filename + "-wal"
	largeLogBytes := walHeaderBytes + walLargeLogPages*(pageSize+walFrameHeaderBytes)

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
				// Logical pages can shrink when the writer starts a new WAL cycle
				// even though the old file allocation is still held on disk.
				var fileBytes int64
				info, statErr := os.Stat(logPath)
				if statErr == nil {
					fileBytes = info.Size()
				} else if errors.Is(statErr, os.ErrNotExist) {
					statErr = nil
				}
				checkErr := errors.Join(err, statErr)
				if checkErr != nil && previousErr == nil && lifetime.Err() == nil {
					slog.Warn("SQLite WAL maintenance check failed", "error", checkErr)
				}
				previousErr = checkErr
				if err != nil || (pages < walLargeLogPages && fileBytes < largeLogBytes) {
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
					slog.Warn("SQLite WAL could not be reset", "file_bytes", fileBytes,
						"pages", pages, "checkpointed", checkpointed, "busy", busy, "error", err)
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
