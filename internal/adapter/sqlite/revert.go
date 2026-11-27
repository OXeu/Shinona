package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/run"
)

func (r *ConversationStore) RevertTurn(ctx context.Context, ownerID, sessionID, turnID, expectedHeadID string, now time.Time) (run.RevertedTurn, error) {
	var result run.RevertedTurn
	var attachments []runAttachmentRow
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		head, err := readTranscriptHead(ctx, tx.Statement.ConnPool, ownerID, sessionID)
		if errors.Is(err, sql.ErrNoRows) {
			return run.ErrSession
		}
		if err != nil {
			return err
		}
		if err := head.state().CheckRevert(expectedHeadID); err != nil {
			return err
		}
		session := head.Session

		var selected row
		if err := conversationTurns(tx, ownerID, sessionID).Select("id", "sequence", "prompt", "payload").
			Take(&selected, "id = ?", turnID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return run.ErrHistoryChanged
			}
			return err
		}
		attachments, err = readRunAttachments(tx, selected.ID, selected.Payload.Images)
		if err != nil {
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
	if err == nil {
		encodeRunAttachments(result.Images, attachments)
	}
	return result, err
}
