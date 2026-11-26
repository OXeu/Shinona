// Package sqlite implements the application repositories with GORM and SQLite.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"time"

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

func newDatabase(db, read *gorm.DB) *Database {
	connections := pools{db: db, read: read}
	return &Database{
		pools:         connections,
		Accounts:      &AccountStore{pools: connections, sessions: newSessionCache()},
		Conversations: &ConversationStore{pools: connections, latest: newLatestTranscriptCache()},
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

func Open(ctx context.Context, filename string) (*Database, error) {
	writeDSN, readDSN, err := databaseDSNs(filename)
	if err != nil {
		return nil, err
	}
	driver := sqlite.DriverName
	if readDSN != "" {
		driver = walWriterDriver
	}
	db, pool, err := openDB(driver, writeDSN, 1)
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
		return newDatabase(db, db), nil
	}
	read, _, err := openDB(sqlite.DriverName, readDSN, min(max(runtime.GOMAXPROCS(0), 2), 16))
	if err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	database := newDatabase(db, read)
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

func openDB(driver, dsn string, maxConnections int) (*gorm.DB, *sql.DB, error) {
	pool, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, nil, err
	}
	pool.SetMaxOpenConns(maxConnections)
	pool.SetMaxIdleConns(maxConnections)
	db, err := gorm.Open(sqlite.New(sqlite.Config{Conn: pool}), &gorm.Config{Logger: timingLogger{logger.Default.LogMode(logger.Silent)}})
	if err != nil {
		return nil, nil, errors.Join(err, pool.Close())
	}
	return db, pool, nil
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
