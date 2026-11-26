package sqlite

import (
	"context"
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

func seedAccounts(t testing.TB, repo *Repository, owners ...string) {
	t.Helper()
	for _, owner := range owners {
		user := auth.User{ID: owner, Email: owner + "@example.test"}
		if err := repo.CreatePasswordUser(t.Context(), user, []byte("test-hash")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFileDatabaseReadsCommittedDataWhileWriting(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "shinona.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	seedAccounts(t, repo, "owner")
	if err := repo.CreateSession(ctx, run.Session{ID: "session", OwnerID: "owner", Title: "before"}); err != nil {
		t.Fatal(err)
	}

	tx := repo.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Model(&sessionRow{}).Where("id = ?", "session").Update("title", "after").Error; err != nil {
		t.Fatal(err)
	}
	assertTitle := func(want string) {
		t.Helper()
		sessions, err := repo.RecentSessions(ctx, "owner", nil, 10)
		if err != nil || len(sessions) != 1 || sessions[0].Title != want {
			t.Fatalf("expected title %q: %+v, %v", want, sessions, err)
		}
	}
	assertTitle("before")
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	assertTitle("after")
	if err := repo.read.WithContext(ctx).Exec("UPDATE agent_sessions SET title = 'read pool write'").Error; err == nil {
		t.Fatal("the read pool accepted a write")
	}
}

func TestSessionCacheObservesOtherRepositoryWrites(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "shinona.db")
	open := func() *Repository {
		t.Helper()
		repo, err := Open(ctx, filename)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = repo.Close() })
		return repo
	}
	repo, other := open(), open()
	user := auth.User{ID: "cached-user", Email: "cached@example.test"}
	now := time.Now().UTC()
	if err := repo.CreatePasswordUser(ctx, user, []byte("test-digest")); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveSession(ctx, "session-key", user.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertAdmin := func(at time.Time, want bool) {
		t.Helper()
		account, err := repo.SessionUser(ctx, "session-key", at)
		if err != nil || account == nil || account.ID != user.ID || account.Admin != want {
			t.Fatalf("unexpected account privileges: %+v, %v", account, err)
		}
	}
	assertAdmin(now, false)
	if _, _, ok := repo.sessions.get("session-key", now); !ok {
		t.Fatal("expected the successful session lookup to populate the cache")
	}
	if account, err := repo.SessionUser(ctx, "session-key", now.Add(time.Hour)); err != nil || account != nil {
		t.Fatalf("expired credential resolved to %+v, %v", account, err)
	}
	if err := other.SetAdmin(ctx, user.ID, true); err != nil {
		t.Fatal(err)
	}
	assertAdmin(now.Add(sessionCacheTTL), true)
	if err := other.DeleteSession(ctx, "session-key"); err != nil {
		t.Fatal(err)
	}
	if account, err := repo.SessionUser(ctx, "session-key", now.Add(2*sessionCacheTTL)); err != nil || account != nil {
		t.Fatalf("revoked credential resolved to %+v, %v", account, err)
	}
}

func TestSessionCacheInvalidatesOnlyTheAffectedAccountOrSession(t *testing.T) {
	repo, err := Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now()
	for _, owner := range []string{"alice", "bob"} {
		user := auth.User{ID: owner, Email: owner + "@example.test"}
		if err := repo.CreatePasswordUser(t.Context(), user, []byte("test-digest")); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{owner + "-first", owner + "-second"} {
			if err := repo.SaveSession(t.Context(), key, owner, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if signedIn, err := repo.SessionUser(t.Context(), key, now); err != nil || signedIn == nil {
				t.Fatalf("authenticate %s: %+v, %v", owner, signedIn, err)
			}
		}
	}
	if err := repo.DeleteSession(t.Context(), "alice-first"); err != nil {
		t.Fatal(err)
	}
	if signedIn, err := repo.SessionUser(t.Context(), "alice-first", now); err != nil || signedIn != nil {
		t.Fatalf("revoked token authenticated: %+v, %v", signedIn, err)
	}
	if _, _, hit := repo.sessions.get("alice-second", now); !hit {
		t.Fatal("revoking one token invalidated the account's other token")
	}
	if err := repo.SetAdmin(t.Context(), "alice", true); err != nil {
		t.Fatal(err)
	}
	if signedIn, err := repo.SessionUser(t.Context(), "alice-second", now); err != nil || signedIn == nil || !signedIn.Admin {
		t.Fatalf("account permissions were not invalidated: %+v, %v", signedIn, err)
	}
	if _, _, hit := repo.sessions.get("bob-first", now); !hit {
		t.Fatal("another account lost its cached authentication")
	}
}

func TestHistoryScopesEarlierTurnsAndSkipsCancelledQueueItems(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	seedAccounts(t, repo, "owner", "other")
	newSession(t, repo, "owner", "session")
	newSession(t, repo, "owner", "other")
	newSession(t, repo, "other", "other-session")
	started := time.Now().UTC()
	rows := []row{
		{ID: "ok", OwnerID: "owner", SessionID: "session", Sequence: 1, Status: run.Succeeded, Prompt: "initial", Output: "reply"},
		{ID: "failed", OwnerID: "owner", SessionID: "session", Sequence: 2, Status: run.Failed, Prompt: "failed prompt"},
		{ID: "cancelled", OwnerID: "owner", SessionID: "session", Sequence: 3, Status: run.Stopped, Prompt: "cancelled in queue"},
		{ID: "stopped", OwnerID: "owner", SessionID: "session", Sequence: 4, Status: run.Stopped, StartedAt: &started, Prompt: "stopped while running"},
		{ID: "current", OwnerID: "owner", SessionID: "session", Sequence: 5, Status: run.Running, Prompt: "current prompt"},
		{ID: "other-session", OwnerID: "owner", SessionID: "other", Sequence: 4, Status: run.Failed, Prompt: "from another session"},
		{ID: "other-owner", OwnerID: "other", SessionID: "other-session", Sequence: 4, Status: run.Stopped, StartedAt: &started, Prompt: "from another account"},
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

func TestHistoryStartsAtLatestEligibleCompaction(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	seedAccounts(t, repo, "owner", "other")
	newSession(t, repo, "owner", "session")
	newSession(t, repo, "other", "other-session")
	reverted := time.Now().UTC()
	checkpoint := []byte(`[{"role":"user","content":"Compacted history"}]`)
	rows := []row{
		{ID: "old", OwnerID: "owner", SessionID: "session", Sequence: 1, Status: run.Succeeded, Prompt: "old"},
		{ID: "compact", OwnerID: "owner", SessionID: "session", Sequence: 2, Status: run.Running, Prompt: "compacted"},
		{ID: "failed", OwnerID: "owner", SessionID: "session", Sequence: 3, Status: run.Failed, CompactedContext: checkpoint},
		{ID: "retry", OwnerID: "owner", SessionID: "session", Sequence: 4, RetryOf: "failed", Status: run.Succeeded, Prompt: "retry"},
		{ID: "reverted", OwnerID: "owner", SessionID: "session", Sequence: 5, Status: run.Succeeded, RevertedAt: &reverted, CompactedContext: checkpoint},
		{ID: "other", OwnerID: "other", SessionID: "other-session", Sequence: 5, Status: run.Succeeded, CompactedContext: checkpoint},
	}
	if err := repo.db.WithContext(ctx).Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.Complete(ctx, "compact", run.Completion{Result: run.Result{CompactedContext: checkpoint}, Status: run.Succeeded, UpdatedAt: reverted}); err != nil {
		t.Fatal(err)
	}
	history, err := repo.History(ctx, run.Run{OwnerID: "owner", SessionID: "session", Sequence: 6}, 0)
	if err != nil || len(history) != 2 || history[0].Prompt != "compacted" || !slices.Equal(history[0].CompactedContext, checkpoint) || history[1].Prompt != "retry" {
		t.Fatalf("expected the current branch's checkpoint and later messages: %+v, %v", history, err)
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

func TestOpenMigratesSessionAndQueueIndexes(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "shinona.db")
	old := openSchemaFixture(t, filename, 1)
	seedAccounts(t, old, "owner")
	if err := old.db.Exec(`DROP INDEX idx_sessions_recent;
		DROP INDEX idx_runs_session_status_sequence;
		CREATE INDEX idx_runs_session_status ON runs(session_id, status)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := old.CreateSession(ctx, run.Session{ID: "stored", OwnerID: "owner", Title: "Stored session"}); err != nil {
		t.Fatal(err)
	}
	markUnversioned(t, old)
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
	for _, expected := range []string{"idx_sessions_recent", "COVERING INDEX idx_runs_session_status_sequence"} {
		if !slices.ContainsFunc(plan, func(step planStep) bool { return strings.Contains(step.Detail, expected) }) {
			t.Errorf("expected %s in session list query plan: %+v", expected, plan)
		}
	}
	if slices.ContainsFunc(plan, func(step planStep) bool { return strings.Contains(step.Detail, "TEMP B-TREE") }) {
		t.Errorf("expected the session list index to satisfy the sort: %+v", plan)
	}
	if repo.db.Migrator().HasIndex(&row{}, "idx_runs_session_status") {
		t.Error("the superseded two-column index is still present")
	}
	plan = nil
	err = repo.db.Raw(`EXPLAIN QUERY PLAN
		SELECT candidate.id FROM runs AS candidate
		WHERE candidate.status = ? AND candidate.owner_id <> '' AND candidate.session_id <> ''
		AND NOT EXISTS (
			SELECT 1 FROM runs AS earlier WHERE earlier.session_id = candidate.session_id
			AND earlier.sequence < candidate.sequence AND earlier.status IN ?
		)
		ORDER BY candidate.created_at, candidate.id LIMIT 1`, run.Queued, []run.Status{run.Queued, run.Running}).Scan(&plan).Error
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(plan, func(step planStep) bool {
		return strings.Contains(step.Detail, "SEARCH earlier USING COVERING INDEX idx_runs_session_status_sequence (session_id=? AND status=? AND sequence<?)")
	}) {
		t.Errorf("expected the earlier-turn check to use the covering index: %+v", plan)
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
	seedAccounts(t, repo, "alice", "bob")
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
