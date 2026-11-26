package sqlite

import (
	"context"
	"encoding/json"
	"slices"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

func (r *ConversationStore) TranscriptUpdate(ctx context.Context, owner, id string, window run.LiveTranscriptWindow,
	known map[string]run.TurnVersion,
) (*run.TranscriptUpdate, error) {
	if window == run.LiveLatestPage {
		if len(known) == 0 {
			page, err := r.TranscriptPage(ctx, owner, id, 0)
			if err != nil || page == nil {
				return nil, err
			}
			return updateFromPage(*page, known), nil
		}
		key := latestTranscriptKey{owner: owner, session: id}
		if cached := r.latest.get(key); cached != nil {
			state, hit, err := r.cachedTranscriptState(ctx, key, cached)
			if err != nil || (hit && state == nil) {
				return nil, err
			}
			if hit {
				defer timing.Start(ctx, "transcript_cache_hit")()
				update := updateFromPage(run.Transcript{
					TranscriptState: *state, Turns: cached.turns, NextBeforeSequence: cached.next,
				}, known)
				update.ChangedTurns = cloneTranscriptTurns(update.ChangedTurns)
				if update.LastOptions != nil {
					update.LastOptions.MCPServers = slices.Clone(update.LastOptions.MCPServers)
				}
				return update, nil
			}
		}
	}
	return r.readTranscriptUpdate(ctx, owner, id, window, known)
}

func updateFromPage(page run.Transcript, known map[string]run.TurnVersion) *run.TranscriptUpdate {
	update := &run.TranscriptUpdate{Session: page.Session, Versions: make([]run.TurnVersion, 0, len(page.Turns)),
		NextBeforeSequence: page.NextBeforeSequence, LastOptions: page.LastOptions()}
	if len(known) == 0 {
		update.ChangedTurns = page.Turns
	}
	for _, turn := range page.Turns {
		version := run.TurnVersion{ID: turn.ID, UpdatedAt: turn.UpdatedAt, Status: turn.Status}
		update.Versions = append(update.Versions, version)
		if len(known) > 0 && version.NeedsSync(known) {
			update.ChangedTurns = append(update.ChangedTurns, turn)
		}
	}
	return update
}

func (r *ConversationStore) readTranscriptUpdate(ctx context.Context, owner, id string, window run.LiveTranscriptWindow,
	known map[string]run.TurnVersion,
) (*run.TranscriptUpdate, error) {
	var update *run.TranscriptUpdate
	var changed []transcriptRow
	var options []byte
	// Keep the versions, cursor, and required bodies in the same read snapshot.
	err := r.read.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state, err := transcriptState(tx, owner, id)
		if err != nil || state == nil {
			return err
		}
		update = &run.TranscriptUpdate{Session: state.Session}
		turns := func() *gorm.DB { return conversationTurns(tx, owner, id) }
		var versions []struct {
			run.TurnVersion `gorm:"embedded"`
			Sequence        int
		}
		limit := window.Limit()
		if err := turns().Select("id", "sequence", "updated_at", "status").Order("sequence DESC").Limit(limit + 1).Find(&versions).Error; err != nil {
			return err
		}
		if len(versions) == 0 {
			return nil
		}
		if len(versions) > limit {
			if window == run.LiveFullHistory {
				return run.ErrTranscriptExportLimit
			}
			versions = versions[:limit]
			update.NextBeforeSequence = versions[limit-1].Sequence
		}

		update.Versions = make([]run.TurnVersion, 0, len(versions))
		ids := make([]string, 0, len(versions))
		for _, row := range slices.Backward(versions) {
			update.Versions = append(update.Versions, row.TurnVersion)
			if row.NeedsSync(known) {
				ids = append(ids, row.ID)
			}
		}
		if len(ids) != 0 {
			if err := transcriptQuery(turns()).Where("id IN ?", ids).Order("sequence ASC").Find(&changed).Error; err != nil {
				return err
			}
		}
		if len(changed) == 0 || changed[len(changed)-1].Run.ID != state.HeadID {
			var latest struct{ Options []byte }
			if err := turns().Select("json_extract(payload, '$.options') AS options").Where("id = ?", state.HeadID).Take(&latest).Error; err != nil {
				return err
			}
			options = latest.Options
			update.LastOptions = new(run.Options)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if update == nil || len(update.Versions) == 0 {
		return update, nil
	}
	if len(changed) != 0 {
		update.ChangedTurns, err = decodeTranscriptRows(ctx, changed)
		if err != nil {
			return nil, err
		}
	}
	if update.LastOptions == nil {
		update.LastOptions = &update.ChangedTurns[len(update.ChangedTurns)-1].Payload.Options
	} else if len(options) != 0 {
		if err := json.Unmarshal(options, update.LastOptions); err != nil {
			return nil, err
		}
	}
	return update, nil
}
