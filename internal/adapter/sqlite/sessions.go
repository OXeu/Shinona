package sqlite

import (
	"context"
	"errors"
	"slices"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

type sessionRow struct {
	ID          string    `gorm:"primaryKey;index:idx_sessions_recent,priority:4,sort:desc"`
	OwnerID     string    `gorm:"not null;index:idx_sessions_owner,priority:1;index:idx_sessions_recent,priority:1,where:archived_at IS NULL"`
	ProjectID   string    `gorm:"not null;default:''"`
	Title       string    `gorm:"not null"`
	TurnCount   int       `gorm:"not null"`
	CreatedAt   time.Time `gorm:"not null"`
	UpdatedAt   time.Time `gorm:"not null;index:idx_sessions_owner,priority:2;index:idx_sessions_recent,priority:3,sort:desc"`
	Pinned      bool      `gorm:"not null;default:false;index:idx_sessions_recent,priority:2,sort:desc"`
	CustomTitle bool      `gorm:"not null;default:false"`
	ReadThrough int       `gorm:"not null;default:0"`
	ArchivedAt  *time.Time
	HasActive   bool `gorm:"->;-:migration"`
}

func (sessionRow) TableName() string { return "agent_sessions" }

type settingsRow struct {
	OwnerID string         `gorm:"primaryKey"`
	Policy  sandbox.Policy `gorm:"not null"`
}

func (settingsRow) TableName() string { return "sandbox_settings" }

func (r *Repository) CreateSession(ctx context.Context, session run.Session) error {
	record := sessionRow{
		ID: session.ID, OwnerID: session.OwnerID, ProjectID: session.ProjectID, Title: session.Title, CustomTitle: session.CustomTitle, TurnCount: session.TurnCount,
		CreatedAt: session.CreatedAt, UpdatedAt: session.UpdatedAt, Pinned: session.Pinned,
	}
	db := r.db.WithContext(ctx)
	if session.ProjectID == "" {
		return db.Create(&record).Error
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var project projectRow
		err := tx.Select("id").Take(&project, "owner_id = ? AND id = ?", session.OwnerID, session.ProjectID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return run.ErrProject
		}
		if err != nil {
			return err
		}
		return tx.Create(&record).Error
	})
}

func sessionScope(db *gorm.DB, ownerID string) *gorm.DB {
	return db.Model(&sessionRow{}).Where("owner_id = ? AND archived_at IS NULL", ownerID)
}

func conversationTurns(db *gorm.DB, ownerID, sessionID string) *gorm.DB {
	return db.Model(&row{}).Where("owner_id = ? AND session_id = ? AND reverted_at IS NULL", ownerID, sessionID)
}

func withActivity(query *gorm.DB) *gorm.DB {
	return query.Select(`agent_sessions.*, EXISTS (
		SELECT 1 FROM runs WHERE session_id = agent_sessions.id AND status IN ?
	) AS has_active`, []run.Status{run.Queued, run.Running})
}

func describeSession(record sessionRow) run.Session {
	return run.Session{
		ID: record.ID, OwnerID: record.OwnerID, ProjectID: record.ProjectID, Title: record.Title, CustomTitle: record.CustomTitle, TurnCount: record.TurnCount,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, Pinned: record.Pinned,
		Active: record.HasActive, Unread: !record.HasActive && record.TurnCount > record.ReadThrough,
	}
}

func (r *Repository) RecentSessions(ctx context.Context, ownerID string, projectID *string, limit int) ([]run.Session, error) {
	return r.recentSessions(ctx, ownerID, projectID, limit, 0)
}

func (r *Repository) recentSessions(ctx context.Context, ownerID string, projectID *string, limit, offset int) ([]run.Session, error) {
	var records []sessionRow
	query := sessionScope(r.db.WithContext(ctx), ownerID)
	if projectID != nil {
		query = query.Where("project_id = ?", *projectID)
	}
	err := withActivity(query).
		Order("pinned DESC, updated_at DESC, id DESC").Limit(limit).Offset(offset).Find(&records).Error
	sessions := make([]run.Session, len(records))
	for i, record := range records {
		sessions[i] = describeSession(record)
	}
	return sessions, err
}

func (r *Repository) LatestWorkspaceRun(ctx context.Context, ownerID, sessionID string) (*run.Run, error) {
	db := r.db.WithContext(ctx)
	session := sessionScope(db, ownerID).Select("id").Where("id = ?", sessionID)
	var record row
	err := db.Select("id", "backend", "policy", "machine_id").
		Where("owner_id = ? AND session_id IN (?) AND (started_at IS NOT NULL OR status IN ?)",
			ownerID, session, []run.Status{run.Succeeded, run.Failed}).
		Order("created_at DESC, sequence DESC, id DESC").Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	turn := run.Run(record)
	return &turn, err
}

func (r *Repository) SessionProject(ctx context.Context, ownerID, sessionID string) (string, error) {
	var record sessionRow
	err := sessionScope(r.db.WithContext(ctx), ownerID).Select("project_id").Take(&record, "id = ?", sessionID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", run.ErrSession
	}
	return record.ProjectID, err
}

func (r *Repository) Transcript(ctx context.Context, ownerID, sessionID string, limit int) (*run.Transcript, error) {
	return r.transcript(ctx, ownerID, sessionID, limit, 0)
}

func (r *Repository) transcript(ctx context.Context, ownerID, sessionID string, limit, offset int) (*run.Transcript, error) {
	var result *run.Transcript
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session sessionRow
		err := withActivity(sessionScope(tx, ownerID)).First(&session, "id = ?", sessionID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var turns []row
		query := conversationTurns(tx, ownerID, sessionID).Select(`id, owner_id, session_id, sequence, policy, backend, machine_id, prompt,
			status, output, steering, parts, context_tokens, context_window, token_usage, created_at, started_at, updated_at,
			json_replace(payload, '$.images', (SELECT json_group_array(json_remove(value, '$.data'))
				FROM json_each(payload, '$.images'))) AS payload`).
			Order("sequence DESC").Offset(offset)
		if limit > 0 {
			query = query.Limit(limit)
		}
		if err := query.Find(&turns).Error; err != nil {
			return err
		}
		slices.Reverse(turns)
		result = &run.Transcript{Session: describeSession(session), Turns: make([]run.Run, len(turns))}
		for i, turn := range turns {
			result.Turns[i] = run.Run(turn)
		}
		return nil
	})
	return result, err
}

func (r *Repository) UpdateSession(ctx context.Context, ownerID, sessionID string, changes run.SessionChanges) (run.Session, error) {
	var record sessionRow
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		updates := make(map[string]any)
		if changes.Title != nil {
			updates["title"] = *changes.Title
			updates["custom_title"] = true
		}
		if changes.Pinned != nil {
			updates["pinned"] = *changes.Pinned
		}
		if len(updates) > 0 {
			if err := sessionScope(tx, ownerID).Where("id = ?", sessionID).UpdateColumns(updates).Error; err != nil {
				return err
			}
		}
		err := withActivity(sessionScope(tx, ownerID)).First(&record, "id = ?", sessionID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return run.ErrSession
		}
		return err
	})
	return describeSession(record), err
}

func (r *Repository) ArchiveSession(ctx context.Context, ownerID, sessionID string, now time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record sessionRow
		err := withActivity(sessionScope(tx, ownerID)).First(&record, "id = ?", sessionID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return run.ErrSession
		}
		if err != nil {
			return err
		}
		if describeSession(record).Active {
			return run.ErrActiveRun
		}
		return sessionScope(tx, ownerID).Where("id = ?", sessionID).UpdateColumn("archived_at", now).Error
	})
}

func (r *Repository) MarkSessionRead(ctx context.Context, ownerID, sessionID string, through int, throughID string) error {
	db := r.db.WithContext(ctx)
	seen := conversationTurns(db, ownerID, sessionID).Select("1").
		Where("sequence = ? AND status IN ?", through, []run.Status{run.Succeeded, run.Failed, run.Stopped})
	if throughID != "" {
		seen = seen.Where("id = ?", throughID)
	}
	return sessionScope(db, ownerID).Where("id = ? AND read_through < ?", sessionID, through).
		Where("EXISTS (?)", seen).UpdateColumn("read_through", through).Error
}

func (r *Repository) Enqueue(ctx context.Context, item run.Run, firstTitle string, queue bool) (run.Run, error) {
	return r.enqueue(ctx, item, firstTitle, queue, nil)
}

func (r *Repository) EnqueueRetry(ctx context.Context, item run.Run, options run.RetryOptions) (run.Run, error) {
	return r.enqueue(ctx, item, "", false, &options)
}

func (r *Repository) enqueue(ctx context.Context, item run.Run, firstTitle string, queue bool, retry *run.RetryOptions) (run.Run, error) {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session sessionRow
		err := sessionScope(tx, item.OwnerID).First(&session, "id = ?", item.SessionID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return run.ErrSession
		}
		if err != nil {
			return err
		}

		var active int64
		if err := tx.Model(&row{}).Where("session_id = ? AND status IN ?", session.ID, []run.Status{run.Queued, run.Running}).
			Count(&active).Error; err != nil {
			return err
		}
		if active > 0 && !queue {
			return run.ErrActiveRun
		}
		if active >= 8 {
			return run.ErrQueueFull
		}
		if retry != nil {
			var failed row
			err := conversationTurns(tx, item.OwnerID, item.SessionID).Select("prompt", "payload").
				Take(&failed, "id = ? AND sequence = ? AND status = ?", item.RetryOf, session.TurnCount, run.Failed).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return run.ErrRetryTurn
			}
			if err != nil {
				return err
			}
			item.Prompt, item.Payload = failed.Prompt, failed.Payload
			if retry.Selection != nil {
				item.Payload.Options = *retry.Selection
			} else if retry.Model != "" {
				item.Payload.Options.Model = retry.Model
			}
		}

		item.Sequence = session.TurnCount + 1
		record := row(item)
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		updates := map[string]any{"turn_count": item.Sequence, "updated_at": item.UpdatedAt}
		if item.Sequence == 1 && !session.CustomTitle {
			updates["title"] = firstTitle
		}
		return tx.Model(&session).Updates(updates).Error
	})
	return item, err
}

func (r *Repository) StopQueued(ctx context.Context, owner, sessionID, id string, now time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&row{}).
			Where("owner_id = ? AND session_id = ? AND id = ? AND status = ?", owner, sessionID, id, run.Queued).
			Updates(map[string]any{"status": run.Stopped, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return run.ErrNotRunning
		}
		return sessionScope(tx, owner).Where("id = ?", sessionID).UpdateColumn("updated_at", now).Error
	})
}

func (r *Repository) History(ctx context.Context, turn run.Run, limit int) ([]run.Exchange, error) {
	var records []row
	db := r.db.WithContext(ctx)
	// Keep each attempt in the transcript and include only its latest retry in model history.
	retried := conversationTurns(db, turn.OwnerID, turn.SessionID).Select("retry_of").
		Where("sequence <= ? AND retry_of <> ''", turn.Sequence).
		Where("status <> ? OR started_at IS NOT NULL", run.Stopped)
	query := conversationTurns(db, turn.OwnerID, turn.SessionID).Select("prompt", "output", "steering", "payload", "agent_messages").
		Where("sequence < ?", turn.Sequence).
		Where("id NOT IN (?)", retried).
		Where("(status IN ? OR (status = ? AND started_at IS NOT NULL))", []run.Status{run.Succeeded, run.Failed}, run.Stopped).
		Order("sequence DESC")
	if limit > 0 {
		query = query.Limit(limit)
	}
	if err := query.Find(&records).Error; err != nil {
		return nil, err
	}
	history := make([]run.Exchange, len(records))
	for i, record := range records {
		history[len(records)-1-i] = run.Exchange{
			Prompt: record.Prompt, Output: record.Output, Steering: record.Steering, Images: record.Payload.Images,
			AgentMessages: record.AgentMessages,
		}
	}
	return history, nil
}

func (r *Repository) Policy(ctx context.Context, ownerID string) (sandbox.Policy, error) {
	var record settingsRow
	err := r.db.WithContext(ctx).First(&record, "owner_id = ?", ownerID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	return record.Policy, err
}

func (r *Repository) SetPolicy(ctx context.Context, ownerID string, choice sandbox.Policy) error {
	record := settingsRow{OwnerID: ownerID, Policy: choice}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "owner_id"}}, DoUpdates: clause.AssignmentColumns([]string{"policy"}),
	}).Create(&record).Error
}

// AdoptLegacy explicitly assigns unowned runs from the pre-login schema.
func (r *Repository) AdoptLegacy(ctx context.Context, ownerID string) (int, error) {
	var count int
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&userRow{}, "id = ?", ownerID).Error; err != nil {
			return err
		}
		var records []row
		if err := tx.Where("owner_id IS NULL OR owner_id = ''").Find(&records).Error; err != nil {
			return err
		}
		for _, record := range records {
			session := sessionRow{
				ID: record.ID, OwnerID: ownerID, Title: "Imported run", TurnCount: 1,
				CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, ReadThrough: 1,
			}
			if err := tx.Create(&session).Error; err != nil {
				return err
			}
			status := record.Status
			if status == run.Queued || status == run.Running {
				status = run.Failed
			}
			if err := tx.Model(&record).Updates(map[string]any{
				"owner_id": ownerID, "session_id": session.ID, "sequence": 1,
				"status": status, "updated_at": record.UpdatedAt,
			}).Error; err != nil {
				return err
			}
		}
		count = len(records)
		return nil
	})
	return count, err
}
