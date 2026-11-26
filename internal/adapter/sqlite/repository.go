// Package sqlite implements the application repositories with GORM and SQLite.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
)

type row struct {
	ID               string `gorm:"primaryKey"`
	OwnerID          string
	SessionID        string `gorm:"index:idx_turns_session,priority:1;index:idx_runs_session_status_sequence,priority:1;index:idx_runs_retries,priority:1,where:retry_of <> ''"`
	RetryOf          string
	Sequence         int `gorm:"index:idx_turns_session,priority:2;index:idx_runs_session_status_sequence,priority:3;index:idx_runs_retries,priority:2"`
	Policy           sandbox.Policy
	Backend          string
	MachineID        string
	Prompt           string                `gorm:"not null"`
	Payload          run.Payload           `gorm:"serializer:json"`
	Steering         []run.SteeringMessage `gorm:"serializer:json"`
	Parts            []run.TurnPart        `gorm:"serializer:json"`
	AgentMessages    []byte
	CompactedContext []byte
	ContextTokens    int
	ContextWindow    int
	TokenUsage       *run.TokenUsage  `gorm:"serializer:json"`
	LiveContext      run.ContextUsage `gorm:"-"`
	Status           run.Status       `gorm:"not null;index:idx_runs_queue,priority:1;index:idx_runs_session_status_sequence,priority:2"`
	Output           string
	CreatedAt        time.Time `gorm:"not null;index:idx_runs_queue,priority:2;index"`
	StartedAt        *time.Time
	UpdatedAt        time.Time `gorm:"not null"`
	RevertedAt       *time.Time
}

func (row) TableName() string { return "runs" }

type Repository struct {
	db       *gorm.DB
	read     *gorm.DB
	sessions sessionCache
	latest   latestTranscriptCache
}

type timingLogger struct{ logger.Interface }

func (timingLogger) Trace(ctx context.Context, begin time.Time, _ func() (string, int64), _ error) {
	timing.Since(ctx, "sqlite", begin)
}

func Open(ctx context.Context, filename string) (*Repository, error) {
	writeDSN, readDSN, err := databaseDSNs(filename)
	if err != nil {
		return nil, err
	}
	db, pool, err := openDB(writeDSN, 1)
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
	models := []any{&userRow{}, &instanceSettingsRow{}, &deletedAccountRow{}}
	for _, table := range ownedAccountTables {
		models = append(models, table)
	}
	if err := db.WithContext(ctx).AutoMigrate(models...); err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	// The covering index also serves the old index's session and status lookups.
	if err := db.WithContext(ctx).Exec("DROP INDEX IF EXISTS idx_runs_session_status").Error; err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	if err := protectDeletedAccounts(db.WithContext(ctx)); err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	if err := initSessionCache(db.WithContext(ctx)); err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	if err := retainE2BProjectProviders(db.WithContext(ctx)); err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	// Existing queued E2B turns should start with the project workspace after upgrade.
	if err := db.WithContext(ctx).Model(&row{}).Where("backend = ? AND policy = ? AND status = ?", "e2b", sandbox.Fresh, run.Queued).
		UpdateColumn("policy", sandbox.Shared).Error; err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	read := db
	if readDSN != "" {
		read, _, err = openDB(readDSN, min(max(runtime.GOMAXPROCS(0), 2), 16))
		if err != nil {
			return nil, errors.Join(err, pool.Close())
		}
	}
	return &Repository{db: db, read: read}, nil
}

func databaseDSNs(filename string) (write, read string, err error) {
	path, query, _ := strings.Cut(filename, "?")
	if path == "" {
		return filename, "", nil
	}
	options, err := url.ParseQuery(query)
	if err != nil {
		return "", "", err
	}
	options.Set("_busy_timeout", "5000")
	if path == ":memory:" || path == "file::memory:" || options.Get("mode") == "memory" {
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

func openDB(dsn string, maxConnections int) (*gorm.DB, *sql.DB, error) {
	pool, err := sql.Open(sqlite.DriverName, dsn)
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

func (r *Repository) Close() error {
	var readErr error
	if r.read != r.db {
		readErr = closeDB(r.read)
	}
	return errors.Join(readErr, closeDB(r.db))
}

func closeDB(db *gorm.DB) error {
	pool, err := db.DB()
	if err != nil {
		return err
	}
	return pool.Close()
}

func (r *Repository) ClaimNext(ctx context.Context, now time.Time) (*run.Run, error) {
	db := r.db.WithContext(ctx)
	next := db.Table("runs AS candidate").Select("candidate.id").
		Where("candidate.status = ? AND candidate.owner_id <> '' AND candidate.session_id <> ''", run.Queued).
		Where(`NOT EXISTS (
			SELECT 1 FROM runs AS earlier WHERE earlier.session_id = candidate.session_id
			AND earlier.sequence < candidate.sequence AND earlier.status IN ?
		)`, []run.Status{run.Queued, run.Running}).
		Order("candidate.created_at, candidate.id").Limit(1)
	var records []row
	result := db.Model(&records).Clauses(clause.Returning{}).
		Where("id = (?) AND status = ?", next, run.Queued).
		Updates(map[string]any{"status": run.Running, "started_at": now, "updated_at": now})
	if result.Error != nil || len(records) == 0 {
		return nil, result.Error
	}
	item := run.Run(records[0])
	return &item, nil
}

func (r *Repository) Complete(ctx context.Context, id string, completion run.Completion) error {
	steering, err := json.Marshal(completion.Steering)
	if err != nil {
		return err
	}
	parts, err := json.Marshal(completion.Parts)
	if err != nil {
		return err
	}
	var usage any
	if completion.TokenUsage != nil {
		data, err := json.Marshal(completion.TokenUsage)
		if err != nil {
			return err
		}
		usage = string(data)
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item row
		result := tx.Model(&item).Clauses(clause.Returning{Columns: []clause.Column{{Name: "session_id"}}}).
			Where("id = ? AND status = ?", id, run.Running).
			Updates(map[string]any{
				"status": completion.Status, "output": completion.Output, "updated_at": completion.UpdatedAt,
				"steering": string(steering), "parts": string(parts), "context_tokens": completion.ContextTokens,
				"context_window": completion.ContextWindow, "token_usage": usage, "agent_messages": completion.AgentMessages,
				"compacted_context": completion.CompactedContext,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("complete run %s: %w", id, run.ErrNotRunning)
		}
		return tx.Model(&sessionRow{}).Where("id = ?", item.SessionID).Update("updated_at", completion.UpdatedAt).Error
	})
}

func (r *Repository) FailInterrupted(ctx context.Context, now time.Time) error {
	return r.db.WithContext(ctx).Model(&row{}).Where("status = ?", run.Running).
		Updates(map[string]any{"status": run.Failed, "updated_at": now}).Error
}

var _ run.Repository = (*Repository)(nil)
