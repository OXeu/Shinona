package sqlite

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func TestTranscriptStreamBatchesLargeTurnsByBytes(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "large-stream.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	const owner, session = "owner", "session"
	seedAccounts(t, repo, owner)
	newSession(t, repo, owner, session)
	now := time.Now().UTC()
	chunk := strings.Repeat("输出", 400_000)
	turns := []row{
		{ID: "output", Output: chunk},
		{ID: "part", Parts: []run.TurnPart{{Kind: "output", Text: chunk}}},
		{ID: "large", Output: chunk + chunk},
		{ID: "small", Output: "done"},
	}
	for i := range turns {
		turns[i].OwnerID, turns[i].SessionID, turns[i].Sequence = owner, session, i+1
		turns[i].Status, turns[i].CreatedAt, turns[i].UpdatedAt = run.Succeeded, now, now
	}
	if err := repo.db.WithContext(ctx).Create(&turns).Error; err != nil {
		t.Fatal(err)
	}
	var batches []int
	var streamed []string
	err = repo.Conversations.StreamTranscript(ctx, owner, session, func(stream run.TranscriptStream) error {
		for {
			batch, err := stream.Next()
			if err != nil || batch == nil {
				return err
			}
			batches = append(batches, len(batch))
			for _, turn := range batch {
				streamed = append(streamed, turn.ID)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(batches, []int{1, 1, 1, 1}) || !slices.Equal(streamed, []string{"output", "part", "large", "small"}) {
		t.Fatalf("export returned batches %v and turns %v", batches, streamed)
	}
}

func TestTranscriptStreamReadsOrderedBatchesFromOneSnapshot(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "stream.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC()
	const owner, session = "owner", "session"
	seedAccounts(t, repo, owner)
	if err := repo.Conversations.CreateSession(ctx, run.Session{ID: session, OwnerID: owner, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	const count = 2*run.TranscriptPageSize + 1
	for i := 1; i <= count; i++ {
		item := run.Run{ID: fmt.Sprint(i), OwnerID: owner, SessionID: session, Prompt: "fixture",
			Status: run.Queued, CreatedAt: now, UpdatedAt: now, Payload: run.Payload{Options: run.Options{Model: fmt.Sprint(i)}}}
		if _, err := repo.Conversations.Enqueue(ctx, item, "Fixture", false); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Conversations.ClaimNext(ctx, now); err != nil {
			t.Fatal(err)
		}
		if err := repo.Conversations.Complete(ctx, item.ID, run.Completion{Status: run.Succeeded, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := repo.read.DB()
	if err != nil {
		t.Fatal(err)
	}
	var batches, sequence int
	err = repo.Conversations.StreamTranscript(ctx, owner, session, func(stream run.TranscriptStream) error {
		if inUse := reader.Stats().InUse; inUse != 0 {
			t.Fatalf("export retained %d database readers while writing", inUse)
		}
		if stream.Session == nil || stream.TurnCount != count || stream.LastOptions == nil || stream.LastOptions.Model != fmt.Sprint(count) {
			t.Fatalf("unexpected transcript header: %+v", stream)
		}
		for {
			page, err := stream.Next()
			if page == nil || err != nil {
				return err
			}
			if inUse := reader.Stats().InUse; inUse != 0 {
				t.Fatalf("batch retained %d database readers while writing", inUse)
			}
			batches++
			if len(page) > run.TranscriptPageSize {
				t.Fatalf("stream collected %d turns in one batch", len(page))
			}
			for _, turn := range page {
				sequence++
				if turn.Sequence != sequence {
					t.Fatalf("turn %s has sequence %d, expected %d", turn.ID, turn.Sequence, sequence)
				}
			}
			if batches == 1 {
				// Commit a revert on the separate writer while the export is
				// reading. The remaining batches must keep the old snapshot.
				if _, err := repo.Conversations.RevertTurn(ctx, owner, session, fmt.Sprint(count-1), fmt.Sprint(count), now); err != nil {
					return err
				}
			}
		}
	})
	if err != nil || batches != 3 || sequence != count {
		t.Fatalf("stream ended after %d batches and %d turns: %v", batches, sequence, err)
	}
	current, err := repo.Conversations.Transcript(ctx, owner, session, count)
	if err != nil || current == nil || len(current.Turns) != count-2 {
		t.Fatalf("reverted transcript did not commit independently: %+v, %v", current, err)
	}
}
