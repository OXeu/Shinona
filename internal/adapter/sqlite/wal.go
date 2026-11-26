package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/mattn/go-sqlite3"
	"gorm.io/driver/sqlite"
)

const (
	walWriterDriver       = "shinona-sqlite-writer"
	walCheckpointInterval = 250 * time.Millisecond
)

func init() {
	sql.Register(walWriterDriver, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			// The threshold is per connection, including replacement writers.
			_, err := conn.Exec("PRAGMA wal_autocheckpoint = 0", nil)
			return err
		},
	})
}

// startWALCheckpoint moves checkpoint I/O to a dedicated connection. PASSIVE
// checkpoints can make progress alongside requests and retry on the next tick.
func startWALCheckpoint(ctx context.Context, dsn string) (func() error, error) {
	pool, err := sql.Open(sqlite.DriverName, dsn)
	if err != nil {
		return nil, err
	}
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
		for {
			select {
			case <-lifetime.Done():
				return
			case <-ticker.C:
				_, err := pool.ExecContext(lifetime, "PRAGMA wal_checkpoint(PASSIVE)")
				if err != nil && previousErr == nil && lifetime.Err() == nil {
					slog.Warn("SQLite WAL checkpoint failed", "error", err)
				}
				previousErr = err
			}
		}
	}()
	return func() error {
		cancel()
		<-stopped
		return pool.Close()
	}, nil
}
