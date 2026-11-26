package sqlite

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func TestTranscriptStreamReadsOrderedBatchesFromOneSnapshot(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "stream.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC()
	const owner, session = "owner", "session"
	if err := repo.CreateSession(ctx, run.Session{ID: session, OwnerID: owner, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	const count = 2*run.TranscriptPageSize + 1
	for i := 1; i <= count; i++ {
		item := run.Run{ID: fmt.Sprint(i), OwnerID: owner, SessionID: session, Prompt: "fixture",
			Status: run.Queued, CreatedAt: now, UpdatedAt: now, Payload: run.Payload{Options: run.Options{Model: fmt.Sprint(i)}}}
		if _, err := repo.Enqueue(ctx, item, "Fixture", false); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.ClaimNext(ctx, now); err != nil {
			t.Fatal(err)
		}
		if err := repo.Complete(ctx, item.ID, run.Completion{Status: run.Succeeded, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	var batches, sequence int
	err = repo.StreamTranscript(ctx, owner, session, func(stream run.TranscriptStream) error {
		if stream.Session == nil || stream.TurnCount != count || stream.LastOptions == nil || stream.LastOptions.Model != fmt.Sprint(count) {
			t.Fatalf("unexpected transcript header: %+v", stream)
		}
		for {
			page, err := stream.Next()
			if page == nil || err != nil {
				return err
			}
			batches++
			if len(page.Turns) > run.TranscriptPageSize {
				t.Fatalf("stream collected %d turns in one batch", len(page.Turns))
			}
			for _, turn := range page.Turns {
				sequence++
				if turn.Sequence != sequence {
					t.Fatalf("turn %s has sequence %d, expected %d", turn.ID, turn.Sequence, sequence)
				}
			}
			if batches == 1 {
				// Commit a revert on the separate writer while the export is
				// reading. The remaining batches must keep the old snapshot.
				if _, err := repo.RevertTurn(ctx, owner, session, fmt.Sprint(count-1), fmt.Sprint(count), now); err != nil {
					return err
				}
			}
		}
	})
	if err != nil || batches != 3 || sequence != count {
		t.Fatalf("stream ended after %d batches and %d turns: %v", batches, sequence, err)
	}
	current, err := repo.Transcript(ctx, owner, session, count)
	if err != nil || current == nil || len(current.Turns) != count-2 {
		t.Fatalf("reverted transcript did not commit independently: %+v, %v", current, err)
	}
}
