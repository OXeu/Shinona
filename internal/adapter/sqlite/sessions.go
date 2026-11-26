package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

type ConversationStore struct {
	pools
	latest latestTranscriptCache
}

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

func (r *ConversationStore) CreateSession(ctx context.Context, session run.Session) error {
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

const sessionWithActivity = `agent_sessions.*, EXISTS (
	SELECT 1 FROM runs WHERE session_id = agent_sessions.id AND status IN ?
) AS has_active`

func withActivity(query *gorm.DB) *gorm.DB {
	return query.Select(sessionWithActivity, []run.Status{run.Queued, run.Running})
}

func describeSession(record sessionRow) run.Session {
	return run.Session{
		ID: record.ID, OwnerID: record.OwnerID, ProjectID: record.ProjectID, Title: record.Title, CustomTitle: record.CustomTitle, TurnCount: record.TurnCount,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, Pinned: record.Pinned,
		Active: record.HasActive, Unread: !record.HasActive && record.TurnCount > record.ReadThrough,
	}
}

func listRecentSessions(db *gorm.DB, ownerID string, projectID *string, limit int) ([]run.Session, error) {
	var records []sessionRow
	query := sessionScope(db, ownerID)
	if projectID != nil {
		query = query.Where("project_id = ?", *projectID)
	}
	err := withActivity(query).
		Order("pinned DESC, updated_at DESC, id DESC").Limit(limit).Find(&records).Error
	sessions := make([]run.Session, len(records))
	for i, record := range records {
		sessions[i] = describeSession(record)
	}
	return sessions, err
}

func (r *ConversationStore) SessionProject(ctx context.Context, ownerID, sessionID string) (string, error) {
	return sessionProject(r.read.WithContext(ctx), ownerID, sessionID)
}

func sessionProject(db *gorm.DB, ownerID, sessionID string) (string, error) {
	var record sessionRow
	err := sessionScope(db, ownerID).Select("project_id").Take(&record, "id = ?", sessionID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", run.ErrSession
	}
	return record.ProjectID, err
}

func (r *ConversationStore) Transcript(ctx context.Context, ownerID, sessionID string, limit int) (*run.Transcript, error) {
	return r.transcript(ctx, ownerID, sessionID, transcriptWindow{limit: limit})
}

func (r *ConversationStore) TranscriptPage(ctx context.Context, ownerID, sessionID string, beforeSequence int) (*run.Transcript, error) {
	window := transcriptWindow{limit: run.TranscriptPageSize, before: beforeSequence, paged: true}
	if beforeSequence != 0 {
		return r.transcript(ctx, ownerID, sessionID, window)
	}
	key := latestTranscriptKey{owner: ownerID, session: sessionID}
	if cached := r.latest.get(key); cached != nil {
		if page, hit, err := r.validateCachedTranscript(ctx, key, cached); hit || err != nil {
			return page, err
		}
	}
	page, err := r.transcript(ctx, ownerID, sessionID, window)
	if err == nil && page != nil && !page.Session.Active && len(page.Turns) > 0 {
		r.latest.put(key, page)
	}
	return page, err
}

type transcriptWindow struct {
	limit  int
	before int
	paged  bool
}

// Keep JSON as bytes until the read transaction has released its connection.
type transcriptRow struct {
	Run        row `gorm:"embedded"`
	Payload    []byte
	Steering   []byte
	Parts      []byte
	TokenUsage []byte
}

func (record *transcriptRow) turn() (run.Run, error) {
	turn := run.Run(record.Run)
	for _, field := range []struct {
		data []byte
		dest any
	}{
		{record.Payload, &turn.Payload},
		{record.Steering, &turn.Steering},
		{record.Parts, &turn.Parts},
		{record.TokenUsage, &turn.TokenUsage},
	} {
		if len(field.data) != 0 {
			if err := json.Unmarshal(field.data, field.dest); err != nil {
				return run.Run{}, fmt.Errorf("decode transcript turn %s: %w", turn.ID, err)
			}
		}
	}
	return turn, nil
}

func (r *ConversationStore) transcript(ctx context.Context, ownerID, sessionID string, window transcriptWindow) (*run.Transcript, error) {
	var result *run.Transcript
	var turns []transcriptRow
	err := r.read.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state, err := transcriptState(tx, ownerID, sessionID)
		if err != nil || state == nil {
			return err
		}
		query := transcriptQuery(conversationTurns(tx, ownerID, sessionID)).Order("sequence DESC")
		if window.before != 0 {
			query = query.Where("sequence < ?", window.before)
		}
		if window.limit > 0 {
			query = query.Limit(window.limit)
		}
		if err := query.Find(&turns).Error; err != nil {
			return err
		}
		result = &run.Transcript{TranscriptState: *state}
		if window.paged && len(turns) == window.limit {
			before := turns[len(turns)-1].Run.Sequence
			var earlier []int
			if err := conversationTurns(tx, ownerID, sessionID).Where("sequence < ?", before).
				Limit(1).Pluck("sequence", &earlier).Error; err != nil {
				return err
			}
			if len(earlier) != 0 {
				result.NextBeforeSequence = before
			}
		}
		return nil
	})
	if err != nil || result == nil {
		return nil, err
	}
	slices.Reverse(turns)
	result.Turns, err = decodeTranscriptRows(ctx, turns)
	return result, err
}

func transcriptQuery(query *gorm.DB) *gorm.DB {
	return query.Select(`id, owner_id, session_id, sequence, policy, backend, prompt,
		status, output, steering, parts, context_tokens, context_window, token_usage, created_at, started_at, updated_at, payload`)
}

func decodeTranscriptRows(ctx context.Context, turns []transcriptRow) ([]run.Run, error) {
	defer timing.Start(ctx, "transcript_json")()
	result := make([]run.Run, len(turns))
	for i := range turns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		turn, err := turns[i].turn()
		if err != nil {
			return nil, err
		}
		result[i] = turn
	}
	return result, nil
}

func (r *ConversationStore) UpdateSession(ctx context.Context, ownerID, sessionID string, changes run.SessionChanges) (run.Session, error) {
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

func (r *ConversationStore) ArchiveSession(ctx context.Context, ownerID, sessionID string, now time.Time) error {
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

func (r *ConversationStore) MarkSessionRead(ctx context.Context, ownerID, sessionID string, through int, throughID string) error {
	db := r.db.WithContext(ctx)
	seen := conversationTurns(db, ownerID, sessionID).Select("1").
		Where("sequence = ? AND status IN ?", through, []run.Status{run.Succeeded, run.Failed, run.Stopped})
	if throughID != "" {
		seen = seen.Where("id = ?", throughID)
	}
	return sessionScope(db, ownerID).Where("id = ? AND read_through < ?", sessionID, through).
		Where("EXISTS (?)", seen).UpdateColumn("read_through", through).Error
}

func (r *ConversationStore) Enqueue(ctx context.Context, item run.Run, firstTitle string, queue bool) (run.Run, error) {
	return r.enqueue(ctx, item, firstTitle, queue, nil)
}

func (r *ConversationStore) EnqueueRetry(ctx context.Context, item run.Run, options run.RetryOptions) (run.Run, error) {
	return r.enqueue(ctx, item, "", false, &options)
}

func (r *ConversationStore) enqueue(ctx context.Context, item run.Run, firstTitle string, queue bool, retry *run.RetryOptions) (run.Run, error) {
	defer timing.Start(ctx, "sqlite_enqueue")()
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
		if err := run.CheckTurnAdmission(active, queue); err != nil {
			return err
		}
		if retry != nil {
			var previous transcriptRow
			err := conversationTurns(tx, item.OwnerID, item.SessionID).Select("id", "sequence", "status", "prompt", "payload").
				Take(&previous, "id = ?", item.RetryOf).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return run.ErrRetryTurn
			}
			if err != nil {
				return err
			}
			if err := describeSession(session).CheckRetry(previous.Run.Sequence, previous.Run.Status); err != nil {
				return err
			}
			original, err := previous.turn()
			if err != nil {
				return err
			}
			records := [...]row{row(original)}
			if err := hydrateRunAttachments(tx, records[:]); err != nil {
				return err
			}
			item.Prompt, item.Payload = records[0].Prompt, records[0].Payload
			item.Payload.Options = retry.Apply(item.Payload.Options)
		}

		item.Sequence = session.TurnCount + 1
		record := row(item)
		record.Payload.Images = slices.Clone(record.Payload.Images)
		for i := range record.Payload.Images {
			record.Payload.Images[i].Data = ""
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		if err := insertRunAttachments(tx, item.ID, item.Payload.Images); err != nil {
			return err
		}
		updates := map[string]any{"turn_count": item.Sequence, "updated_at": item.UpdatedAt}
		if title := describeSession(session).TitleAfterAppend(firstTitle); title != session.Title {
			updates["title"] = title
		}
		return tx.Model(&session).Updates(updates).Error
	})
	return item, err
}

func (r *ConversationStore) StopQueued(ctx context.Context, owner, sessionID, id string, now time.Time) error {
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

func (r *ConversationStore) TakeQueued(ctx context.Context, owner, sessionID, id string, textOnly bool, now time.Time) (run.PendingDraft, error) {
	var draft run.PendingDraft
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session sessionRow
		if err := sessionScope(tx, owner).Take(&session, "id = ?", sessionID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return run.ErrSession
			}
			return err
		}
		queued := func() *gorm.DB {
			return conversationTurns(tx, owner, sessionID).Where("id = ? AND status = ?", id, run.Queued)
		}
		var items [1]row
		item := &items[0]
		if err := queued().Select("id", "sequence", "prompt", "payload").Take(item).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return run.ErrNotRunning
			}
			return err
		}
		if textOnly && len(item.Payload.Images) > 0 {
			return run.ErrImages
		}
		if err := hydrateRunAttachments(tx, items[:]); err != nil {
			return err
		}
		result := queued().Delete(&row{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return run.ErrNotRunning
		}
		updates := map[string]any{"updated_at": now}
		if item.Sequence == session.TurnCount {
			sequence, err := lastTurnSequence(tx, owner, sessionID)
			if err != nil {
				return err
			}
			updates["turn_count"] = sequence
			updates["read_through"] = min(session.ReadThrough, sequence)
			if title := describeSession(session).TitleAfterRemoval(sequence); title != session.Title {
				updates["title"] = title
			}
		}
		draft = run.PendingDraft{Prompt: item.Prompt, Images: item.Payload.Images}
		return sessionScope(tx, owner).Where("id = ?", sessionID).UpdateColumns(updates).Error
	})
	return draft, err
}

func lastTurnSequence(db *gorm.DB, owner, sessionID string) (int, error) {
	var last row
	err := conversationTurns(db, owner, sessionID).Select("sequence").Order("sequence DESC").Take(&last).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = nil
	}
	return last.Sequence, err
}

func (r *ConversationStore) History(ctx context.Context, turn run.Run, limit int) ([]run.Exchange, error) {
	var records []row
	err := r.read.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := historyQuery(tx, turn, limit).Find(&records).Error; err != nil {
			return err
		}
		return hydrateRunAttachments(tx, records)
	})
	if err != nil {
		return nil, err
	}
	history := make([]run.Exchange, len(records))
	for i, record := range records {
		history[len(records)-1-i] = run.Exchange{
			Prompt: record.Prompt, Output: record.Output, Steering: record.Steering, Images: record.Payload.Images,
			AgentMessages: record.AgentMessages, CompactedContext: record.CompactedContext,
		}
	}
	return history, nil
}

func historyQuery(db *gorm.DB, turn run.Run, limit int) *gorm.DB {
	// Keep each attempt in the transcript and include only its latest retry in model history.
	retried := conversationTurns(db, turn.OwnerID, turn.SessionID).Select("retry_of").
		Where("sequence <= ? AND retry_of <> ''", turn.Sequence).
		Where("status <> ? OR started_at IS NOT NULL", run.Stopped)
	eligibleTurns := func() *gorm.DB {
		return conversationTurns(db, turn.OwnerID, turn.SessionID).
			Where("sequence < ?", turn.Sequence).
			Where("id NOT IN (?)", retried).
			Where("(status IN ? OR (status = ? AND started_at IS NOT NULL))", []run.Status{run.Succeeded, run.Failed}, run.Stopped)
	}
	query := eligibleTurns().Select("id", "prompt", "output", "steering", "payload", "agent_messages", "compacted_context").
		Order("sequence DESC")
	if limit > 0 {
		query = query.Limit(limit)
	} else {
		checkpoint := eligibleTurns().Select("sequence").Where("compacted_context IS NOT NULL").Order("sequence DESC").Limit(1)
		query = query.Where("sequence >= COALESCE((?), 0)", checkpoint)
	}
	return query
}

// AdoptLegacy explicitly assigns unowned runs from the pre-login schema.
func (r *ConversationStore) AdoptLegacy(ctx context.Context, ownerID string) (int, error) {
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
