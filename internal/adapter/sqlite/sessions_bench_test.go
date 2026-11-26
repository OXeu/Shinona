package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/OXeu/shinona/internal/run"
)

func BenchmarkSessionsList(b *testing.B) {
	ctx := b.Context()
	repo, err := Open(ctx, filepath.Join(b.TempDir(), "sessions.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = repo.Close() })
	transferFixture{sessions: 2_000, turns: 1}.seed(b, repo)
	service := run.NewService(repo, nil, nil, &run.Controls{}, nil)

	b.ReportAllocs()
	for b.Loop() {
		items, err := service.RecentSessions(ctx, transferOwner, nil)
		if err != nil || len(items) != 30 {
			b.Fatalf("load recent sessions: got %d, want 30 (%v)", len(items), err)
		}
	}
}
