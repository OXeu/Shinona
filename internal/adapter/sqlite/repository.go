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
	db             *gorm.DB
	read           *gorm.DB
	sessions       sessionCache
	latest         latestTranscriptCache
	stopCheckpoint func() error
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
	repo := &Repository{db: db, read: db}
	if readDSN == "" {
		return repo, nil
	}
	repo.read, _, err = openDB(sqlite.DriverName, readDSN, min(max(runtime.GOMAXPROCS(0), 2), 16))
	if err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	repo.stopCheckpoint, err = startWALCheckpoint(ctx, readDSN)
	if err != nil {
		return nil, errors.Join(err, repo.Close())
	}
	return repo, nil
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

func (r *Repository) Close() error {
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

func (r *Repository) ClaimNext(ctx context.Context, now time.Time) (*run.Run, error) {
	var records []row
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		records, err = claimNext(tx, now)
		if err != nil || len(records) == 0 {
			return err
		}
		return hydrateRunAttachments(tx, records)
	})
	if err != nil || len(records) == 0 {
		return nil, err
	}
	item := run.Run(records[0])
	return &item, nil
}

func claimNext(db *gorm.DB, now time.Time) ([]row, error) {
	next := db.Table("runs AS candidate").Select("candidate.id").
		Joins("LEFT JOIN agent_sessions AS workspace ON workspace.id = candidate.session_id AND workspace.owner_id = candidate.owner_id").
		Where("candidate.status = ? AND candidate.owner_id <> '' AND candidate.session_id <> ''", run.Queued).
		Where(`NOT EXISTS (
			SELECT 1 FROM runs AS earlier WHERE earlier.session_id = candidate.session_id
			AND earlier.sequence < candidate.sequence AND earlier.status IN ?
		)`, []run.Status{run.Queued, run.Running}).
		Where(`NOT EXISTS (
			SELECT 1 FROM runs AS executing
			JOIN agent_sessions AS active_session ON active_session.id = executing.session_id
				AND active_session.owner_id = executing.owner_id
			WHERE executing.status = 'RUNNING' AND executing.owner_id = candidate.owner_id
				AND active_session.project_id = workspace.project_id
		)`).
		Order("candidate.created_at, candidate.id").Limit(1)
	var records []row
	result := db.Model(&records).Clauses(clause.Returning{}).
		Where("id = (?) AND status = ?", next, run.Queued).
		Updates(map[string]any{"status": run.Running, "started_at": now, "updated_at": now})
	return records, result.Error
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
