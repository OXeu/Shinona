package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/run"
)

func (r *Repository) Attachment(ctx context.Context, ownerID, sessionID, turnID string, index int) (*run.Image, error) {
	db := r.read.WithContext(ctx)
	// Keep image bytes out of the transcript and read only the requested attachment.
	path := "$.images[" + strconv.Itoa(index) + "]"
	var record struct{ Image *string }
	err := attachmentTurn(db, ownerID, sessionID, turnID).
		Select("json_extract(payload, ?) AS image", path).
		Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil || record.Image == nil {
		return nil, err
	}
	var image run.Image
	if err := json.Unmarshal([]byte(*record.Image), &image); err != nil {
		return nil, err
	}
	return &image, nil
}

func (r *Repository) AttachmentTurnVisible(ctx context.Context, ownerID, sessionID, turnID string) (bool, error) {
	var found struct{ ID string }
	err := attachmentTurn(r.read.WithContext(ctx), ownerID, sessionID, turnID).Select("id").Take(&found).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}

func attachmentTurn(db *gorm.DB, ownerID, sessionID, turnID string) *gorm.DB {
	session := sessionScope(db, ownerID).Select("id").Where("id = ?", sessionID)
	return conversationTurns(db, ownerID, sessionID).Where("id = ? AND session_id IN (?)", turnID, session)
}
