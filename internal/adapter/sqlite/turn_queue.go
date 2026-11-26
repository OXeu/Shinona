package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
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

func (r *ConversationStore) ClaimNext(ctx context.Context, now time.Time) (*run.Run, error) {
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

func (r *ConversationStore) Complete(ctx context.Context, id string, completion run.Completion) error {
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

func (r *ConversationStore) FailInterrupted(ctx context.Context, now time.Time) error {
	return r.db.WithContext(ctx).Model(&row{}).Where("status = ?", run.Running).
		Updates(map[string]any{"status": run.Failed, "updated_at": now}).Error
}

var _ run.WorkerRepository = (*ConversationStore)(nil)
