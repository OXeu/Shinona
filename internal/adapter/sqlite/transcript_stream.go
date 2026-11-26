package sqlite

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/run"
)

const transcriptBatchBytes = 4 << 20

type exportTurnRef struct {
	ID   string
	Size int64
}

func (r *ConversationStore) StreamTranscript(ctx context.Context, owner, id string, write func(run.TranscriptStream) error) error {
	var stream run.TranscriptStream
	var refs []exportTurnRef
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
		// Measure the variable-size fields without pulling their bodies into Go.
		// SQLite can read octet_length for text and blobs from the record header.
		const columns = `id, coalesce(octet_length(prompt), 0) + coalesce(octet_length(output), 0)
			+ coalesce(octet_length(parts), 0) + coalesce(octet_length(steering), 0)
			+ coalesce(octet_length(payload), 0) + coalesce(octet_length(token_usage), 0) AS size`
		if err := turns().Select(columns).Order("sequence ASC, id ASC").Limit(run.MaxTranscriptExportTurns + 1).Find(&refs).Error; err != nil {
			return err
		}
		if len(refs) > run.MaxTranscriptExportTurns {
			return run.ErrTranscriptExportLimit
		}
		if len(refs) == 0 {
			return nil
		}
		stream.HeadID = refs[len(refs)-1].ID
		var latest struct{ Options []byte }
		if err := turns().Select("json_extract(payload, '$.options') AS options").
			Where("id = ?", stream.HeadID).Take(&latest).Error; err != nil {
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
	stream.TurnCount = len(refs)
	stream.Next = func() ([]run.Run, error) {
		if len(refs) == 0 {
			return nil, nil
		}
		// Allow a large individual turn to be exported on its own. The wire
		// limit still applies to the entire export, including that turn.
		count, remaining := 1, transcriptBatchBytes-refs[0].Size
		for count < min(run.TranscriptPageSize, len(refs)) && refs[count].Size <= remaining {
			remaining -= refs[count].Size
			count++
		}
		batch := refs[:count]
		refs = refs[count:]
		turns := make(map[string]transcriptRow, len(batch))
		completed := make([]string, 0, len(batch))
		for _, ref := range batch {
			if turn, ok := frozen[ref.ID]; ok {
				turns[ref.ID] = turn
			} else {
				completed = append(completed, ref.ID)
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
		for i, ref := range batch {
			ordered[i] = turns[ref.ID]
		}
		return decodeTranscriptRows(ctx, ordered)
	}
	return write(stream)
}
