package sqlite

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/run"
)

type runAttachmentRow struct {
	TurnID   string `gorm:"primaryKey"`
	Position int    `gorm:"primaryKey;autoIncrement:false"`
	Content  []byte
}

func (runAttachmentRow) TableName() string { return "run_attachments" }

func migrateRunAttachments(db *gorm.DB) error {
	if err := db.Exec(`CREATE TABLE run_attachments (
		turn_id TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
		position INTEGER NOT NULL,
		content BLOB NOT NULL,
		PRIMARY KEY (turn_id, position)
	) WITHOUT ROWID`).Error; err != nil {
		return err
	}
	var after string
	for {
		var record row
		err := db.Select("id", "payload").Where("id > ? AND json_array_length(payload, '$.images') > 0", after).
			Order("id ASC").Take(&record).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var paths []any
		for position, image := range record.Payload.Images {
			if image.Data == "" {
				continue
			}
			paths = append(paths, fmt.Sprintf("$.images[%d].data", position))
		}
		if len(paths) > 0 {
			if err := insertRunAttachments(db, record.ID, record.Payload.Images); err != nil {
				return err
			}
			expression := "json_remove(payload" + strings.Repeat(", ?", len(paths)) + ")"
			if err := db.Model(&record).UpdateColumn("payload", gorm.Expr(expression, paths...)).Error; err != nil {
				return err
			}
		}
		after = record.ID
	}
}

func insertRunAttachments(db *gorm.DB, turn string, images []run.Image) error {
	var records []runAttachmentRow
	for position, image := range images {
		if image.Data == "" {
			continue
		}
		content, err := base64.StdEncoding.DecodeString(image.Data)
		if err != nil {
			return fmt.Errorf("decode turn %s attachment %d: %w", turn, position, err)
		}
		records = append(records, runAttachmentRow{TurnID: turn, Position: position, Content: content})
	}
	if len(records) == 0 {
		return nil
	}
	return db.Create(&records).Error
}

func hydrateRunAttachments(db *gorm.DB, turns []row) error {
	imagesByTurn := make(map[string][]run.Image)
	var ids []string
	for _, turn := range turns {
		if len(turn.Payload.Images) != 0 {
			imagesByTurn[turn.ID] = turn.Payload.Images
			ids = append(ids, turn.ID)
		}
	}
	for start := 0; start < len(ids); start += 256 {
		var attachments []runAttachmentRow
		if err := db.Where("turn_id IN ?", ids[start:min(start+256, len(ids))]).Find(&attachments).Error; err != nil {
			return err
		}
		for _, attachment := range attachments {
			images := imagesByTurn[attachment.TurnID]
			if attachment.Position < 0 || attachment.Position >= len(images) {
				return fmt.Errorf("invalid attachment position for turn %s", attachment.TurnID)
			}
			images[attachment.Position].Data = base64.StdEncoding.EncodeToString(attachment.Content)
		}
	}
	return nil
}

func (r *Repository) Attachment(ctx context.Context, ownerID, sessionID, turnID string, index int) (*run.Image, error) {
	if index < 0 || index >= run.MaxImages {
		return nil, nil
	}
	db := r.read.WithContext(ctx)
	path := fmt.Sprintf("$.images[%d]", index)
	var record struct {
		Image   *string
		Content []byte
	}
	err := attachmentTurn(db, ownerID, sessionID, turnID).
		Select("json_extract(runs.payload, ?) AS image, attachment.content", path).
		Joins("JOIN run_attachments AS attachment ON attachment.turn_id = runs.id AND attachment.position = ?", index).
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
	image.Data = base64.StdEncoding.EncodeToString(record.Content)
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
