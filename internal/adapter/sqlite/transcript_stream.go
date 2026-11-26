package sqlite

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/run"
)

func (r *Repository) StreamTranscript(ctx context.Context, owner, id string, write func(run.TranscriptStream) error) error {
	return r.read.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row sessionRow
		err := withActivity(sessionScope(tx, owner)).First(&row, "id = ?", id).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return write(run.TranscriptStream{})
		}
		if err != nil {
			return err
		}
		query := conversationTurns(tx, owner, id)
		var count int64
		candidates := query.Session(&gorm.Session{}).Select("1").Limit(run.MaxTranscriptExportTurns + 1)
		if err := tx.Table("(?) AS candidates", candidates).Count(&count).Error; err != nil {
			return err
		}
		if count > run.MaxTranscriptExportTurns {
			return run.ErrTranscriptExportLimit
		}
		var options *run.Options
		if count != 0 {
			var latest struct{ Options []byte }
			err := query.Session(&gorm.Session{}).Select("json_extract(payload, '$.options') AS options").
				Order("sequence DESC, id DESC").Take(&latest).Error
			if err != nil {
				return err
			}
			options = new(run.Options)
			if len(latest.Options) != 0 {
				if err := json.Unmarshal(latest.Options, options); err != nil {
					return err
				}
			}
		}

		remaining := int(count)
		var sequence int
		var turnID string
		next := func() (*run.TranscriptSnapshot, error) {
			if remaining == 0 {
				return nil, nil
			}
			page := transcriptQuery(tx, owner, id).Order("sequence ASC, id ASC").Limit(run.TranscriptPageSize)
			if turnID != "" {
				page = page.Where("(sequence, id) > (?, ?)", sequence, turnID)
			}
			var rows []transcriptRow
			if err := page.Find(&rows).Error; err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				return nil, errors.New("transcript snapshot ended before its recorded turn count")
			}
			chunk := &run.TranscriptSnapshot{}
			if err := decodeTranscriptRows(ctx, chunk, rows, true, false); err != nil {
				return nil, err
			}
			last := rows[len(rows)-1].Run
			sequence, turnID = last.Sequence, last.ID
			remaining -= len(rows)
			return chunk, nil
		}
		session := describeSession(row)
		return write(run.TranscriptStream{Session: &session, LastOptions: options, TurnCount: int(count), Next: next})
	})
}
