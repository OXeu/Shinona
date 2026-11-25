package sqlite

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

func TestHistoryScopesEarlierTurnsAndSkipsCancelledQueueItems(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	started := time.Now().UTC()
	rows := []row{
		{ID: "ok", OwnerID: "owner", SessionID: "session", Sequence: 1, Status: run.Succeeded, Prompt: "initial", Output: "reply"},
		{ID: "failed", OwnerID: "owner", SessionID: "session", Sequence: 2, Status: run.Failed, Prompt: "failed prompt"},
		{ID: "cancelled", OwnerID: "owner", SessionID: "session", Sequence: 3, Status: run.Stopped, Prompt: "cancelled in queue"},
		{ID: "stopped", OwnerID: "owner", SessionID: "session", Sequence: 4, Status: run.Stopped, StartedAt: &started, Prompt: "stopped while running"},
		{ID: "current", OwnerID: "owner", SessionID: "session", Sequence: 5, Status: run.Running, Prompt: "current prompt"},
		{ID: "other-session", OwnerID: "owner", SessionID: "other", Sequence: 4, Status: run.Failed, Prompt: "from another session"},
		{ID: "other-owner", OwnerID: "other", SessionID: "session", Sequence: 4, Status: run.Stopped, StartedAt: &started, Prompt: "from another account"},
	}
	if err := repo.db.WithContext(ctx).Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	history, err := repo.History(ctx, run.Run(rows[4]), 3)
	if err != nil {
		t.Fatal(err)
	}
	prompts := make([]string, len(history))
	for i, exchange := range history {
		prompts[i] = exchange.Prompt
	}
	if !slices.Equal(prompts, []string{"initial", "failed prompt", "stopped while running"}) || history[0].Output != "reply" {
		t.Fatalf("unexpected model history: %+v", history)
	}
}

func TestLegacyRunsStayPrivateAndCanBeAdopted(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "shinona.db")
	old, err := sql.Open("sqlite3", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.ExecContext(ctx, `
		CREATE TABLE runs (
			id text primary key, prompt text not null, status text not null, output text,
			created_at datetime not null, updated_at datetime not null
		);
		INSERT INTO runs VALUES
			('finished', 'original prompt', 'SUCCEEDED', 'original output', '2026-01-01', '2026-01-01'),
			('pending', 'old queued prompt', 'QUEUED', '', '2026-01-01', '2026-01-01');
	`)
	if err != nil {
		_ = old.Close()
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	repo, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	}()
	owner := auth.User{ID: "user-id", Email: "person@example.test"}
	if err := repo.CreatePasswordUser(ctx, owner, []byte("test-digest")); err != nil {
		t.Fatal(err)
	}

	if claimed, err := repo.ClaimNext(ctx, time.Now().UTC()); err != nil || claimed != nil {
		t.Fatalf("unowned run should stay off the queue: %+v, %v", claimed, err)
	}
	if items, err := repo.RecentSessions(ctx, owner.ID, nil, 10); err != nil || len(items) != 0 {
		t.Fatalf("unowned run should stay private: %+v, %v", items, err)
	}
	if count, err := repo.AdoptLegacy(ctx, owner.ID); err != nil || count != 2 {
		t.Fatalf("adopt legacy runs: %d, %v", count, err)
	}
	if count, err := repo.AdoptLegacy(ctx, owner.ID); err != nil || count != 0 {
		t.Fatalf("repeat adoption: %d, %v", count, err)
	}
	finished, err := repo.Transcript(ctx, owner.ID, "finished", 10)
	if err != nil || finished == nil || len(finished.Turns) != 1 ||
		finished.Turns[0].Output != "original output" || finished.Turns[0].Prompt != "original prompt" {
		t.Fatalf("existing result was lost: %+v, %v", finished, err)
	}
	pending, err := repo.Transcript(ctx, owner.ID, "pending", 10)
	if err != nil || pending == nil || len(pending.Turns) != 1 || pending.Turns[0].Status != run.Failed {
		t.Fatalf("old unfinished run should be imported as failed: %+v, %v", pending, err)
	}
	if other, err := repo.Transcript(ctx, "other-owner", "finished", 10); err != nil || other != nil {
		t.Fatalf("migration exposed cross-account data: %+v, %v", other, err)
	}

	if err := repo.SetPolicy(ctx, owner.ID, sandbox.Shared); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	if policy, err := reopened.Policy(ctx, owner.ID); err != nil || policy != sandbox.Shared {
		t.Fatalf("preference was not persisted: %q, %v", policy, err)
	}
}

func TestOpenAddsSessionListIndexesToExistingDatabase(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "shinona.db")
	old, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = old.Close() })
	if err := old.db.Exec("DROP INDEX idx_sessions_recent; DROP INDEX idx_runs_session_status").Error; err != nil {
		t.Fatal(err)
	}
	if err := old.CreateSession(ctx, run.Session{ID: "stored", OwnerID: "owner", Title: "Stored session"}); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	repo, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	sessions, err := repo.RecentSessions(ctx, "owner", nil, 30)
	if err != nil || len(sessions) != 1 || sessions[0].ID != "stored" {
		t.Fatalf("recent sessions after upgrade: %+v, %v", sessions, err)
	}

	type planStep struct{ Detail string }
	var plan []planStep
	err = repo.db.Raw(`EXPLAIN QUERY PLAN
		SELECT agent_sessions.*, EXISTS (SELECT 1 FROM runs WHERE session_id = agent_sessions.id AND status IN ?) AS has_active
		FROM agent_sessions WHERE owner_id = ? AND archived_at IS NULL
		ORDER BY pinned DESC, updated_at DESC, id DESC LIMIT 30`, []run.Status{run.Queued, run.Running}, "owner").Scan(&plan).Error
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"idx_sessions_recent", "COVERING INDEX idx_runs_session_status"} {
		if !slices.ContainsFunc(plan, func(step planStep) bool { return strings.Contains(step.Detail, expected) }) {
			t.Errorf("expected %s in session list query plan: %+v", expected, plan)
		}
	}
	if slices.ContainsFunc(plan, func(step planStep) bool { return strings.Contains(step.Detail, "TEMP B-TREE") }) {
		t.Errorf("expected the session list index to satisfy the sort: %+v", plan)
	}
}

func TestSessionActionsAndUnreadTransitions(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	}()

	now := time.Now().UTC()
	entry := run.Session{ID: "work", OwnerID: "alice", Title: "New session", CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateSession(ctx, entry); err != nil {
		t.Fatal(err)
	}
	title, pinned := "Workbench", true
	if _, err := repo.UpdateSession(ctx, "bob", entry.ID, run.SessionChanges{Title: &title}); !errors.Is(err, run.ErrSession) {
		t.Fatalf("cross-account rename: %v", err)
	}
	if _, err := repo.UpdateSession(ctx, "alice", entry.ID, run.SessionChanges{Title: &title, Pinned: &pinned}); err != nil {
		t.Fatal(err)
	}
	turn := run.Run{
		ID: "turn", OwnerID: "alice", SessionID: entry.ID, Prompt: "First prompt",
		Status: run.Queued, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := repo.Enqueue(ctx, turn, "Automatic title", false); err != nil {
		t.Fatal(err)
	}
	assertSessionFlags(t, repo, true, false)
	if err := repo.ArchiveSession(ctx, "alice", entry.ID, now); !errors.Is(err, run.ErrActiveRun) {
		t.Fatalf("archive while active: %v", err)
	}
	if err := repo.MarkSessionRead(ctx, "alice", entry.ID, 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ClaimNext(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Complete(ctx, turn.ID, run.Completion{Status: run.Succeeded, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	assertSessionFlags(t, repo, false, true)
	if err := repo.MarkSessionRead(ctx, "bob", entry.ID, 1, ""); err != nil {
		t.Fatal(err)
	}
	assertSessionFlags(t, repo, false, true)
	if err := repo.MarkSessionRead(ctx, "alice", entry.ID, 1, ""); err != nil {
		t.Fatal(err)
	}
	assertSessionFlags(t, repo, false, false)
	if err := repo.ArchiveSession(ctx, "alice", entry.ID, now); err != nil {
		t.Fatal(err)
	}
	if session, err := repo.Transcript(ctx, "alice", entry.ID, 10); err != nil || session != nil {
		t.Fatalf("archived session still accessible: %+v, %v", session, err)
	}
	var count int64
	if err := repo.db.WithContext(ctx).Model(&row{}).Where("id = ?", turn.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("archive did not retain the turn: count=%d, %v", count, err)
	}
}

func assertSessionFlags(t *testing.T, repo *Repository, active, unread bool) {
	t.Helper()
	sessions, err := repo.RecentSessions(t.Context(), "alice", nil, 10)
	if err != nil || len(sessions) != 1 || !sessions[0].Pinned || sessions[0].Title != "Workbench" ||
		sessions[0].Active != active || sessions[0].Unread != unread {
		t.Fatalf("expected active=%v unread=%v, got %+v and %v", active, unread, sessions, err)
	}
}
