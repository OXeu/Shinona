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
	db := r.db.WithContext(ctx)
	// Keep image bytes out of the transcript and read only the requested attachment.
	path := "$.images[" + strconv.Itoa(index) + "]"
	session := sessionScope(db, ownerID).Select("id").Where("id = ?", sessionID)
	var record struct{ Image *string }
	err := conversationTurns(db, ownerID, sessionID).
		Select("json_extract(payload, ?) AS image", path).
		Where("session_id IN (?)", session).Take(&record, "id = ?", turnID).Error
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
