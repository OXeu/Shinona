package sqlite

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/run"
)

func (r *ConversationStore) StreamTranscript(ctx context.Context, owner, id string, write func(run.TranscriptStream) error) error {
	var stream run.TranscriptStream
	var ids []string
	var pending []transcriptRow
	err := r.read.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record sessionRow
		err := withActivity(sessionScope(tx, owner)).First(&record, "id = ?", id).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		session := describeSession(record)
		stream.Session = &session

		// Completed contents are immutable. Capture membership, ordering and
		// mutable turns together, then release the transaction before writing.
		turns := func() *gorm.DB { return conversationTurns(tx, owner, id) }
		if err := turns().Order("sequence ASC, id ASC").Limit(run.MaxTranscriptExportTurns+1).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) > run.MaxTranscriptExportTurns {
			return run.ErrTranscriptExportLimit
		}
		if len(ids) == 0 {
			return nil
		}
		var latest struct{ Options []byte }
		if err := turns().Select("json_extract(payload, '$.options') AS options").
			Where("id = ?", ids[len(ids)-1]).Take(&latest).Error; err != nil {
			return err
		}
		stream.LastOptions = new(run.Options)
		if len(latest.Options) != 0 {
			if err := json.Unmarshal(latest.Options, stream.LastOptions); err != nil {
				return err
			}
		}
		if session.Active {
			return transcriptQuery(turns()).Where("status IN ?", []run.Status{run.Queued, run.Running}).Find(&pending).Error
		}
		return nil
	})
	if err != nil {
		return err
	}
	if stream.Session == nil {
		return write(stream)
	}
	frozen := make(map[string]transcriptRow, len(pending))
	for _, turn := range pending {
		frozen[turn.Run.ID] = turn
	}
	stream.TurnCount = len(ids)
	stream.Next = func() ([]run.Run, error) {
		if len(ids) == 0 {
			return nil, nil
		}
		batch := ids[:min(run.TranscriptPageSize, len(ids))]
		ids = ids[len(batch):]
		turns := make(map[string]transcriptRow, len(batch))
		var completed []string
		for _, id := range batch {
			if turn, ok := frozen[id]; ok {
				turns[id] = turn
			} else {
				completed = append(completed, id)
			}
		}
		if len(completed) != 0 {
			query := r.read.WithContext(ctx).Model(&row{}).Where("owner_id = ? AND session_id = ? AND id IN ?", owner, id, completed)
			var records []transcriptRow
			if err := transcriptQuery(query).Find(&records).Error; err != nil {
				return nil, err
			}
			for _, row := range records {
				turns[row.Run.ID] = row
			}
		}
		if len(turns) != len(batch) {
			return nil, errors.New("a transcript turn was deleted during the export")
		}
		ordered := make([]transcriptRow, len(batch))
		for i, id := range batch {
			ordered[i] = turns[id]
		}
		return decodeTranscriptRows(ctx, ordered)
	}
	return write(stream)
}
