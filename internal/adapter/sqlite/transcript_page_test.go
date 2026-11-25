package sqlite

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func TestTranscriptPageSequence(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	transferFixture{sessions: 1, turns: 65}.seed(t, repo)

	check := func(before, first, last, next int) *run.Transcript {
		t.Helper()
		page, err := repo.TranscriptPage(ctx, transferOwner, transferSession, before)
		if err != nil || page == nil {
			t.Fatalf("get page before %d: %v", before, err)
		}
		if len(page.Turns) != last-first+1 || page.NextBeforeSequence != next {
			t.Fatalf("before %d: got %d turns, next = %d", before, len(page.Turns), page.NextBeforeSequence)
		}
		for i, turn := range page.Turns {
			if turn.Sequence != first+i {
				t.Fatalf("turn %d: got sequence %d, want %d", i, turn.Sequence, first+i)
			}
		}
		return page
	}
	latest := check(0, 36, 65, 36)
	if latest.Session.TurnCount != 65 {
		t.Fatalf("total turn count: %d", latest.Session.TurnCount)
	}
	now := time.Now().UTC()
	if _, err := repo.Enqueue(ctx, run.Run{
		ID: "appended", OwnerID: transferOwner, SessionID: transferSession, Status: run.Queued,
		CreatedAt: now, UpdatedAt: now,
	}, "", false); err != nil {
		t.Fatal(err)
	}
	older := check(latest.NextBeforeSequence, 6, 35, 6)
	check(older.NextBeforeSequence, 1, 5, 0)
	check(0, 37, 66, 37)
	check(31, 1, 30, 0)
	if err := repo.StopQueued(ctx, transferOwner, transferSession, "appended", now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RevertTurn(ctx, transferOwner, transferSession, older.Turns[0].ID, "appended", now); err != nil {
		t.Fatal(err)
	}
	check(0, 1, 5, 0)
	if page, err := repo.TranscriptPage(ctx, "other-owner", transferSession, 36); err != nil || page != nil {
		t.Fatalf("cursor must not expose another owner's session: %v, %v", page, err)
	}
}

func BenchmarkTranscriptPages(b *testing.B) {
	for _, count := range []int{120, 500} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			ctx := b.Context()
			repo, err := Open(ctx, filepath.Join(b.TempDir(), "transcript.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = repo.Close() })
			transferFixture{sessions: 1, turns: count, textBytes: 8 << 10}.seed(b, repo)
			for _, scenario := range []struct {
				name         string
				before, want int
			}{
				{name: "full", want: count},
				{name: "latest", want: run.TranscriptPageSize},
				{name: "deep", before: 2*run.TranscriptPageSize + 1, want: run.TranscriptPageSize},
			} {
				b.Run(scenario.name, func(b *testing.B) {
					read := func() (*run.Transcript, error) {
						if scenario.name == "full" {
							return repo.Transcript(ctx, transferOwner, transferSession, 0)
						}
						return repo.TranscriptPage(ctx, transferOwner, transferSession, scenario.before)
					}
					page, err := read()
					if err != nil || page == nil || len(page.Turns) != scenario.want {
						b.Fatalf("load fixture transcript: %v", err)
					}
					b.ReportAllocs()
					for b.Loop() {
						if _, err := read(); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
