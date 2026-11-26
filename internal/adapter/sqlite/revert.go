package sqlite

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/run"
)

func (r *ConversationStore) RevertTurn(ctx context.Context, ownerID, sessionID, turnID, expectedHeadID string, now time.Time) (run.RevertedTurn, error) {
	var result run.RevertedTurn
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session sessionRow
		if err := withActivity(sessionScope(tx, ownerID)).Take(&session, "id = ?", sessionID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return run.ErrSession
			}
			return err
		}
		if session.HasActive {
			return run.ErrActiveRun
		}
		var head row
		err := conversationTurns(tx, ownerID, sessionID).Select("id").
			Order("sequence DESC").Take(&head).Error
		if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && head.ID != expectedHeadID) {
			return run.ErrHistoryChanged
		}
		if err != nil {
			return err
		}

		var items [1]row
		selected := &items[0]
		if err := conversationTurns(tx, ownerID, sessionID).Select("id", "sequence", "prompt", "payload").
			Take(selected, "id = ?", turnID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return run.ErrHistoryChanged
			}
			return err
		}
		if err := hydrateRunAttachments(tx, items[:]); err != nil {
			return err
		}
		if err := conversationTurns(tx, ownerID, sessionID).
			Where("sequence >= ?", selected.Sequence).UpdateColumn("reverted_at", now).Error; err != nil {
			return err
		}
		session.TurnCount, err = lastTurnSequence(tx, ownerID, sessionID)
		if err != nil {
			return err
		}
		session.ReadThrough = min(session.ReadThrough, session.TurnCount)
		session.UpdatedAt = now
		session.Title = describeSession(session).TitleAfterRemoval(session.TurnCount)
		if err := sessionScope(tx, ownerID).Where("id = ?", sessionID).UpdateColumns(map[string]any{
			"turn_count": session.TurnCount, "read_through": session.ReadThrough,
			"updated_at": session.UpdatedAt, "title": session.Title,
		}).Error; err != nil {
			return err
		}
		result = run.RevertedTurn{Session: describeSession(session), Prompt: selected.Prompt, Images: selected.Payload.Images}
		return nil
	})
	return result, err
}
