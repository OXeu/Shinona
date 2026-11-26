// Package sqlite implements the application repositories with GORM and SQLite.
package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

// Database owns the SQLite connection pools and the stores using them.
type Database struct {
	pools
	Accounts       *AccountStore
	Conversations  *ConversationStore
	Projects       *ProjectStore
	Settings       *SettingsStore
	E2B            *E2BStore
	stopCheckpoint func() error
}

type pools struct {
	db   *gorm.DB
	read *gorm.DB
}

func newDatabase(db, read *gorm.DB, transcriptCacheBytes int) *Database {
	connections := pools{db: db, read: read}
	return &Database{
		pools:         connections,
		Accounts:      &AccountStore{pools: connections, sessions: newSessionCache()},
		Conversations: &ConversationStore{pools: connections, latest: newLatestTranscriptCache(transcriptCacheBytes)},
		Projects:      &ProjectStore{pools: connections},
		Settings:      &SettingsStore{pools: connections},
		E2B:           &E2BStore{pools: connections},
	}
}

func (d *Database) RunStores() run.ServiceStores {
	return run.ServiceStores{
		Sessions:    d.Conversations,
		Transcripts: d.Conversations,
		Turns:       d.Conversations,
		Projects:    d.Projects,
		Preferences: d.Settings,
	}
}

type timingLogger struct{ logger.Interface }

func (timingLogger) Trace(ctx context.Context, begin time.Time, _ func() (string, int64), _ error) {
	timing.Since(ctx, "sqlite", begin)
}

const MaxReadConnections = 64

type Config struct {
	ReadConnections      int
	TranscriptCacheBytes int
}

func DefaultConfig() Config {
	return Config{ReadConnections: min(max(runtime.GOMAXPROCS(0), 2), 16), TranscriptCacheBytes: 32 << 20}
}

func Open(ctx context.Context, filename string) (*Database, error) {
	return OpenWithConfig(ctx, filename, DefaultConfig())
}

// OpenWithConfig sets the read-only pool size and decoded transcript cache budget.
// In-memory databases use a single connection for both reads and writes.
func OpenWithConfig(ctx context.Context, filename string, config Config) (*Database, error) {
	if config.ReadConnections < 1 || config.ReadConnections > MaxReadConnections {
		return nil, fmt.Errorf("SQLite read connections must be between 1 and %d", MaxReadConnections)
	}
	if config.TranscriptCacheBytes < 0 {
		return nil, errors.New("SQLite transcript cache budget must be nonnegative")
	}
	writeDSN, readDSN, err := databaseDSNs(filename)
	if err != nil {
		return nil, err
	}
	writer := &sqliteConnector{dsn: writeDSN}
	if readDSN != "" {
		writer.driver.ConnectHook = disableWALAutoCheckpoint
	}
	pool := sql.OpenDB(writer)
	db, err := openDB(pool, 1)
	if err != nil {
		return nil, err
	}
	if readDSN != "" {
		var journal string
		if err := db.WithContext(ctx).Raw("PRAGMA journal_mode").Row().Scan(&journal); err != nil {
			return nil, errors.Join(err, pool.Close())
		}
		if journal != "wal" {
			return nil, errors.Join(fmt.Errorf("SQLite journal mode is %q, expected WAL", journal), pool.Close())
		}
	}
	if err := migrate(db.WithContext(ctx)); err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	if readDSN == "" {
		return newDatabase(db, db, config.TranscriptCacheBytes), nil
	}
	read, err := openDB(sql.OpenDB(&sqliteConnector{dsn: readDSN}), config.ReadConnections)
	if err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	database := newDatabase(db, read, config.TranscriptCacheBytes)
	database.stopCheckpoint, err = startWALCheckpoint(ctx, readDSN)
	if err != nil {
		return nil, errors.Join(err, database.Close())
	}
	return database, nil
}

func databaseDSNs(filename string) (write, read string, err error) {
	path, query, _ := strings.Cut(filename, "?")
	options, err := url.ParseQuery(query)
	if err != nil {
		return "", "", err
	}
	options.Set("_busy_timeout", "5000")
	options.Del("_fk")
	options.Set("_foreign_keys", "1")
	options.Set("_ignore_check_constraints", "0")
	if path == "" || path == ":memory:" || path == "file::memory:" || options.Get("mode") == "memory" {
		return path + "?" + options.Encode(), "", nil
	}

	options.Del("_journal")
	options.Del("_sync")
	options.Set("_journal_mode", "WAL")
	options.Set("_synchronous", "NORMAL")
	options.Set("_txlock", "immediate")
	write = path + "?" + options.Encode()
	options.Del("_journal_mode")
	options.Set("_txlock", "deferred")
	options.Set("_query_only", "1")
	return write, path + "?" + options.Encode(), nil
}

// sqliteConnector keeps driver options scoped to their connection pool.
type sqliteConnector struct {
	dsn    string
	driver sqlite3.SQLiteDriver
}

func (c *sqliteConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.driver.Open(c.dsn)
}

func (c *sqliteConnector) Driver() driver.Driver {
	return &c.driver
}

func openDB(pool *sql.DB, maxConnections int) (*gorm.DB, error) {
	pool.SetMaxOpenConns(maxConnections)
	pool.SetMaxIdleConns(maxConnections)
	db, err := gorm.Open(sqlite.New(sqlite.Config{Conn: pool}), &gorm.Config{Logger: timingLogger{logger.Default.LogMode(logger.Silent)}})
	if err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	return db, nil
}

func (r *Database) Close() error {
	var checkpointErr, readErr error
	if r.stopCheckpoint != nil {
		checkpointErr = r.stopCheckpoint()
	}
	if r.read != r.db {
		readErr = closeDB(r.read)
	}
	return errors.Join(checkpointErr, readErr, closeDB(r.db))
}

func closeDB(db *gorm.DB) error {
	pool, err := db.DB()
	if err != nil {
		return err
	}
	return pool.Close()
}
