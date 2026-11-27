package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

func seedAccounts(t testing.TB, repo *Database, owners ...string) {
	t.Helper()
	for _, owner := range owners {
		user := auth.User{ID: owner, Email: owner + "@example.test"}
		if err := repo.Accounts.CreatePasswordUser(t.Context(), user, []byte("test-hash")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFileDatabaseReadsCommittedDataWhileWriting(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	const maxReaders = 3
	config := DefaultConfig()
	config.ReadConnections = maxReaders
	repo, err := OpenWithConfig(ctx, filepath.Join(t.TempDir(), "shinona.db"), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	writer, err := repo.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := repo.read.DB()
	if err != nil {
		t.Fatal(err)
	}
	if writer.Stats().MaxOpenConnections != 1 || reader.Stats().MaxOpenConnections != maxReaders {
		t.Fatalf("file database limits: writer %d, reader %d", writer.Stats().MaxOpenConnections, reader.Stats().MaxOpenConnections)
	}
	seedAccounts(t, repo, "owner")
	if err := repo.Conversations.CreateSession(ctx, run.Session{ID: "session", OwnerID: "owner", Title: "before"}); err != nil {
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
		list, err := repo.Conversations.SessionListSnapshot(ctx, "owner", nil, 10)
		sessions := list.Sessions
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
	for range maxReaders {
		conn, err := reader.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		var queryOnly int
		if err := conn.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
			t.Fatalf("read connection query_only = %d, %v", queryOnly, err)
		}
	}
	if got := reader.Stats().InUse; got != maxReaders {
		t.Fatalf("simultaneous read connections = %d, want %d", got, maxReaders)
	}
}

func TestMemoryDatabaseKeepsOneSharedConnection(t *testing.T) {
	for _, dsn := range []string{":memory:", "file::memory:", "file:configured-memory?mode=memory"} {
		t.Run(dsn, func(t *testing.T) {
			config := DefaultConfig()
			config.ReadConnections = MaxReadConnections
			repo, err := OpenWithConfig(t.Context(), dsn, config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			reader, err := repo.read.DB()
			if err != nil {
				t.Fatal(err)
			}
			if stats := reader.Stats(); repo.db != repo.read || stats.MaxOpenConnections != 1 || repo.stopCheckpoint != nil {
				t.Fatalf("in-memory database did not share a single connection: %+v", stats)
			}
			seedAccounts(t, repo, "owner")
			if err := repo.Conversations.CreateSession(t.Context(), run.Session{ID: "session", OwnerID: "owner"}); err != nil {
				t.Fatal(err)
			}
			list, err := repo.Conversations.SessionListSnapshot(t.Context(), "owner", nil, 1)
			sessions := list.Sessions
			if err != nil || len(sessions) != 1 || sessions[0].ID != "session" {
				t.Fatalf("in-memory read did not see the write: %v, %v", sessions, err)
			}
		})
	}
}

func TestMemoryDatabaseSurvivesCanceledTransaction(t *testing.T) {
	for _, dsn := range []string{":memory:", "file::memory:", "file:configured-memory?mode=memory"} {
		t.Run(dsn, func(t *testing.T) {
			ctx, stop := context.WithTimeout(t.Context(), 2*time.Second)
			defer stop()
			repo, err := Open(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			seedAccounts(t, repo, "owner")
			if err := repo.Conversations.CreateSession(ctx, run.Session{ID: "session", OwnerID: "owner", Title: "committed"}); err != nil {
				t.Fatal(err)
			}
			pool, err := repo.db.DB()
			if err != nil {
				t.Fatal(err)
			}

			txCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			tx, err := pool.BeginTx(txCtx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(txCtx, "UPDATE agent_sessions SET title = 'canceled' WHERE id = 'session'"); err != nil {
				t.Fatal(err)
			}
			// Leave rollback to database/sql, as when a client disconnects.
			cancel()
			list, err := repo.Conversations.SessionListSnapshot(ctx, "owner", nil, 1)
			if err != nil || len(list.Sessions) != 1 || list.Sessions[0].Title != "committed" {
				t.Fatalf("committed session after automatic rollback: %+v, %v", list.Sessions, err)
			}
		})
	}
}

func TestMemoryDatabaseLifetimeAndIsolation(t *testing.T) {
	for _, fixture := range []struct {
		dsn    string
		shared bool
	}{
		{dsn: ""},
		{dsn: ":memory:?cache=shared"},
		{dsn: "file::memory:"},
		{dsn: "file::memory:?cache=shared", shared: true},
		{dsn: "file:private-memory?mode=memory&cache=private"},
		{dsn: "file:shared-memory?mode=memory&cache=shared", shared: true},
	} {
		t.Run(fixture.dsn, func(t *testing.T) {
			ctx := t.Context()
			open := func() *Database {
				t.Helper()
				db, err := Open(ctx, fixture.dsn)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				return db
			}
			assertSession := func(db *Database, want bool) {
				t.Helper()
				list, err := db.Conversations.SessionListSnapshot(ctx, "owner", nil, 1)
				if err != nil || (len(list.Sessions) == 1) != want {
					t.Fatalf("shared session present = %t, want %t, error = %v", len(list.Sessions) == 1, want, err)
				}
			}
			first := open()
			seedAccounts(t, first, "owner")
			if err := first.Conversations.CreateSession(ctx, run.Session{ID: "session", OwnerID: "owner"}); err != nil {
				t.Fatal(err)
			}
			second := open()
			assertSession(second, fixture.shared)
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			assertSession(second, fixture.shared)
			if err := second.Close(); err != nil {
				t.Fatal(err)
			}
			assertSession(open(), false)
		})
	}
}

func TestSessionCacheObservesOtherRepositoryWrites(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "shinona.db")
	open := func() *Database {
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
	if err := repo.Accounts.CreatePasswordUser(ctx, user, []byte("test-digest")); err != nil {
		t.Fatal(err)
	}
	if err := repo.Accounts.SaveSession(ctx, "session-key", user.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertAdmin := func(at time.Time, want bool) {
		t.Helper()
		account, err := repo.Accounts.SessionUser(ctx, "session-key", at)
		if err != nil || account == nil || account.ID != user.ID || account.Admin != want {
			t.Fatalf("unexpected account privileges: %+v, %v", account, err)
		}
	}
	assertAdmin(now, false)
	if _, _, ok := repo.Accounts.sessions.get("session-key", now); !ok {
		t.Fatal("expected the successful session lookup to populate the cache")
	}
	if account, err := repo.Accounts.SessionUser(ctx, "session-key", now.Add(time.Hour)); err != nil || account != nil {
		t.Fatalf("expired credential resolved to %+v, %v", account, err)
	}
	if err := other.Accounts.SetAdmin(ctx, user.ID, true); err != nil {
		t.Fatal(err)
	}
	assertAdmin(now.Add(sessionCacheTTL), true)
	if err := other.Accounts.DeleteSession(ctx, "session-key"); err != nil {
		t.Fatal(err)
	}
	if account, err := repo.Accounts.SessionUser(ctx, "session-key", now.Add(2*sessionCacheTTL)); err != nil || account != nil {
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
		if err := repo.Accounts.CreatePasswordUser(t.Context(), user, []byte("test-digest")); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{owner + "-first", owner + "-second"} {
			if err := repo.Accounts.SaveSession(t.Context(), key, owner, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if signedIn, err := repo.Accounts.SessionUser(t.Context(), key, now); err != nil || signedIn == nil {
				t.Fatalf("authenticate %s: %+v, %v", owner, signedIn, err)
			}
		}
	}
	if err := repo.Accounts.DeleteSession(t.Context(), "alice-first"); err != nil {
		t.Fatal(err)
	}
	if signedIn, err := repo.Accounts.SessionUser(t.Context(), "alice-first", now); err != nil || signedIn != nil {
		t.Fatalf("revoked token authenticated: %+v, %v", signedIn, err)
	}
	if _, _, hit := repo.Accounts.sessions.get("alice-second", now); !hit {
		t.Fatal("revoking one token invalidated the account's other token")
	}
	if err := repo.Accounts.SetAdmin(t.Context(), "alice", true); err != nil {
		t.Fatal(err)
	}
	if signedIn, err := repo.Accounts.SessionUser(t.Context(), "alice-second", now); err != nil || signedIn == nil || !signedIn.Admin {
		t.Fatalf("account permissions were not invalidated: %+v, %v", signedIn, err)
	}
	if _, _, hit := repo.Accounts.sessions.get("bob-first", now); !hit {
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
	history, err := repo.Conversations.History(ctx, run.Run(rows[4]), 3)
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

func TestDelayedCompletionPreservesNewerSessionActivity(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	const owner, session = "owner", "session"
	seedAccounts(t, repo, owner)
	newSession(t, repo, owner, session)
	now := time.Now().UTC()
	finished := now.Add(time.Second)
	queuedAt := now.Add(2 * time.Second)
	for _, item := range []run.Run{
		{ID: "finishing", OwnerID: owner, SessionID: session, Status: run.Running, CreatedAt: now, UpdatedAt: now},
		{ID: "queued", OwnerID: owner, SessionID: session, Status: run.Queued, CreatedAt: queuedAt, UpdatedAt: queuedAt},
	} {
		if _, err := repo.Conversations.Enqueue(ctx, item, "", true); err != nil {
			t.Fatal(err)
		}
	}
	completion := run.Completion{Status: run.Succeeded, UpdatedAt: finished, Result: run.Result{Output: "done"}}
	if err := repo.Conversations.Complete(ctx, "finishing", completion); err != nil {
		t.Fatal(err)
	}
	list, err := repo.Conversations.SessionListSnapshot(ctx, owner, nil, 1)
	items := list.Sessions
	if err != nil || len(items) != 1 || !items[0].UpdatedAt.Equal(queuedAt) || !items[0].Active {
		t.Fatalf("completion changed more recent session activity: %+v, %v", items, err)
	}
	var stored row
	if err := repo.read.WithContext(ctx).First(&stored, "id = ?", "finishing").Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != run.Succeeded || !stored.UpdatedAt.Equal(finished) || stored.Output != "done" {
		t.Fatalf("unexpected persisted completion: %+v", stored)
	}
}

func TestRetryCopiesStoredAttachmentsInSQLite(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	seedAccounts(t, repo, "owner")
	newSession(t, repo, "owner", "session")
	now := time.Now().UTC()
	images := []run.Image{
		{Name: "stored.png", MediaType: "image/png", Data: "c3RvcmVk"},
		{Name: "legacy.png", MediaType: "image/png", Data: "bGVnYWN5"},
	}
	original := run.Run{
		ID: "original", OwnerID: "owner", SessionID: "session", Prompt: "retry me", Status: run.Failed,
		CreatedAt: now, UpdatedAt: now, Payload: run.Payload{Images: images},
	}
	if _, err := repo.Conversations.Enqueue(ctx, original, "", false); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.Delete(&runAttachmentRow{}, "turn_id = ? AND position = 1", original.ID).Error; err != nil {
		t.Fatal(err)
	}
	err = repo.db.Callback().Create().Before("gorm:create").Register("test:retry-inline-images-only", func(db *gorm.DB) {
		if db.Statement.Table != "run_attachments" {
			return
		}
		images, ok := db.Statement.Dest.(*[]runAttachmentRow)
		if !ok || len(*images) != 1 || (*images)[0].Position != 1 {
			t.Error("only the inline image should be inserted through Go")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	retry := run.Run{ID: "retry", OwnerID: "owner", SessionID: "session", RetryOf: original.ID, Status: run.Queued, CreatedAt: now, UpdatedAt: now}
	if _, err := repo.Conversations.EnqueueRetry(ctx, retry, run.RetryOptions{}); err == nil {
		t.Fatal("retry accepted a missing original image")
	}
	state, err := repo.Conversations.TranscriptState(ctx, retry.OwnerID, retry.SessionID)
	if err != nil || state == nil || state.HeadID != original.ID || state.Session.TurnCount != 1 || state.Session.Active {
		t.Fatalf("failed retry changed the session: %+v, %v", state, err)
	}
	// Exercise an older inline image alongside one already in the BLOB table.
	if err := repo.db.Exec(`UPDATE runs SET payload = json_set(payload, '$.images[1].data', ?) WHERE id = ?`, images[1].Data, original.ID).Error; err != nil {
		t.Fatal(err)
	}
	queued, err := repo.Conversations.EnqueueRetry(ctx, retry, run.RetryOptions{Model: "another-model"})
	if err != nil || queued.Prompt != original.Prompt || queued.Payload.Options.Model != "another-model" || !slices.Equal(queued.Payload.Images, images) {
		t.Fatalf("retry changed the prompt, options or images: %+v, %v", queued, err)
	}
	for index, want := range []string{"stored", "legacy"} {
		attachment, err := repo.Conversations.Attachment(ctx, retry.OwnerID, retry.SessionID, retry.ID, index)
		if err != nil || attachment == nil || string(attachment.Data) != want {
			t.Fatalf("retry image %d was not persisted: %+v, %v", index, attachment, err)
		}
	}
}

func TestClaimNextSkipsWriterWhenNothingCanRun(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	assertIdle := func(t *testing.T) {
		t.Helper()
		tx := repo.db.WithContext(t.Context()).Begin()
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		defer tx.Rollback()
		probe, stop := context.WithTimeout(t.Context(), time.Second)
		defer stop()
		if item, err := repo.Conversations.ClaimNext(probe, time.Now().UTC()); err != nil || item != nil {
			t.Fatalf("empty claim waited for the writer: %+v, %v", item, err)
		}
	}
	t.Run("empty", assertIdle)
	seedAccounts(t, repo, "owner")
	newSession(t, repo, "owner", "running-session")
	newSession(t, repo, "owner", "waiting-session")
	now := time.Now().UTC()
	for _, item := range []run.Run{
		{ID: "running", OwnerID: "owner", SessionID: "running-session", Status: run.Running, CreatedAt: now, UpdatedAt: now},
		{ID: "waiting", OwnerID: "owner", SessionID: "waiting-session", Status: run.Queued, CreatedAt: now, UpdatedAt: now},
	} {
		if _, err := repo.Conversations.Enqueue(ctx, item, "", false); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("busy project", assertIdle)
	if err := repo.Conversations.Complete(ctx, "running", run.Completion{Status: run.Succeeded, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if claim, err := repo.Conversations.ClaimNext(ctx, now); err != nil || claim == nil || claim.ID != "waiting" {
		t.Fatalf("the turn should be eligible when its project becomes available: %+v, %v", claim, err)
	}
}

func TestClaimNextRechecksEligibilityAfterRead(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "queue-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	seedAccounts(t, repo, "owner")
	now := time.Now().UTC()
	for _, id := range []string{"first", "second"} {
		newSession(t, repo, "owner", id)
		item := run.Run{ID: id, OwnerID: "owner", SessionID: id, Status: run.Queued, CreatedAt: now, UpdatedAt: now}
		if _, err := repo.Conversations.Enqueue(ctx, item, "", false); err != nil {
			t.Fatal(err)
		}
	}
	writer, err := repo.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	tx := repo.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Model(&row{}).Where("id = ?", "first").UpdateColumn("status", run.Running).Error; err != nil {
		t.Fatal(err)
	}
	before := writer.Stats().WaitCount
	limit, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	type claimResult struct {
		turn *run.Run
		err  error
	}
	result := make(chan claimResult, 1)
	go func() {
		turn, err := repo.Conversations.ClaimNext(limit, now)
		result <- claimResult{turn, err}
	}()
	// Let the read see the queued turns while the writer still has an
	// uncommitted change that makes their workspace busy.
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for writer.Stats().WaitCount == before {
		select {
		case <-tick.C:
		case got := <-result:
			t.Fatalf("claim did not wait for the writer: %+v, %v", got.turn, got.err)
		case <-limit.Done():
			t.Fatal("claim did not reach the writer pool")
		}
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.err != nil || got.turn != nil {
		t.Fatalf("claimed a turn while its workspace was busy: %+v, %v", got.turn, got.err)
	}
}

func TestClaimNextDefersAttachmentReads(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "claim.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	seedAccounts(t, repo, "owner", "other")
	newSession(t, repo, "owner", "session")
	newSession(t, repo, "other", "other-session")
	now := time.Now().UTC()
	image := run.Image{Name: "fixture.png", MediaType: "image/png", Data: "iVBORw0KGgo="}
	item := run.Run{
		ID: "turn", OwnerID: "owner", SessionID: "session", Status: run.Queued, CreatedAt: now, UpdatedAt: now,
		Payload: run.Payload{Images: []run.Image{image}},
	}
	if _, err := repo.Conversations.Enqueue(ctx, item, "", false); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.WithContext(ctx).Model(&runAttachmentRow{}).Where("turn_id = ?", item.ID).UpdateColumn("position", 1).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := repo.Conversations.ClaimNext(ctx, now.Add(time.Second))
	if err != nil || claimed == nil || claimed.ID != item.ID || claimed.Status != run.Running {
		t.Fatalf("attachment content interfered with claiming the turn: %+v, %v", claimed, err)
	}
	metadata := image
	metadata.Data = ""
	if !slices.Equal(claimed.Payload.Images, []run.Image{metadata}) {
		t.Fatalf("claim must retain only image metadata: %+v", claimed.Payload.Images)
	}
	if _, err := repo.Conversations.LoadTurnImages(ctx, *claimed); err == nil {
		t.Fatal("accepted an invalid image position")
	}
	other := run.Run{
		ID: "other-turn", OwnerID: "other", SessionID: "other-session", Status: run.Queued, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := repo.Conversations.Enqueue(ctx, other, "", false); err != nil {
		t.Fatal(err)
	}
	next, err := repo.Conversations.ClaimNext(ctx, now.Add(2*time.Second))
	if err != nil || next == nil || next.ID != other.ID {
		t.Fatalf("damaged attachment blocked another account's queue: %+v, %v", next, err)
	}
	if err := repo.db.WithContext(ctx).Model(&runAttachmentRow{}).Where("turn_id = ?", item.ID).UpdateColumn("position", 0).Error; err != nil {
		t.Fatal(err)
	}
	foreign := *claimed
	foreign.OwnerID = "other"
	if images, err := repo.Conversations.LoadTurnImages(ctx, foreign); err == nil || len(images) != 0 {
		t.Fatalf("loaded attachments for another account: %+v, %v", images, err)
	}
	images, err := repo.Conversations.LoadTurnImages(ctx, *claimed)
	if err != nil || !slices.Equal(images, []run.Image{image}) || claimed.Payload.Images[0].Data != "" {
		t.Fatalf("load images without changing the claimed turn: %+v, %v", images, err)
	}
}

func TestRecoverInterruptedTurns(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "interrupted.db")
	repo, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if repo != nil {
			_ = repo.Close()
		}
	})
	seedAccounts(t, repo, "alice", "bob")
	started := time.Now().UTC()
	recovered := started.Add(time.Second)
	queuedAt := recovered.Add(time.Second)
	for _, session := range []run.Session{
		{ID: "one", OwnerID: "alice", CreatedAt: started, UpdatedAt: started},
		{ID: "two", OwnerID: "bob", CreatedAt: started, UpdatedAt: started},
	} {
		if err := repo.Conversations.CreateSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	partial := run.TurnPart{Kind: "thinking", Text: "working"}
	for _, item := range []run.Run{
		{ID: "lost", OwnerID: "alice", SessionID: "one", Status: run.Running, CreatedAt: started, UpdatedAt: started},
		{ID: "partial", OwnerID: "bob", SessionID: "two", Status: run.Running, CreatedAt: started, UpdatedAt: started, Parts: []run.TurnPart{partial}},
		{ID: "pending", OwnerID: "bob", SessionID: "two", Status: run.Queued, CreatedAt: queuedAt, UpdatedAt: queuedAt},
	} {
		if _, err := repo.Conversations.Enqueue(ctx, item, "", true); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.db.WithContext(ctx).Model(&row{}).Where("id = ?", "lost").UpdateColumn("parts", "[{broken").Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{recovered, queuedAt.Add(time.Second)} {
		if err := repo.Conversations.FailInterrupted(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	assertTranscript := func(owner, session string, updated time.Time, active bool, parts []run.TurnPart) *run.Transcript {
		t.Helper()
		transcript, err := repo.Conversations.Transcript(ctx, owner, session, 5)
		if err != nil || transcript == nil || len(transcript.Turns) == 0 {
			t.Fatalf("recovered transcript %s: %+v, %v", session, transcript, err)
		}
		if transcript.Session.Active != active || !transcript.Session.UpdatedAt.Equal(updated) {
			t.Errorf("unexpected session activity: %+v", transcript.Session)
		}
		first := transcript.Turns[0]
		if first.Status != run.Failed || !first.UpdatedAt.Equal(recovered) || !slices.Equal(first.Parts, parts) {
			t.Errorf("unexpected recovered turn: %+v", first)
		}
		return transcript
	}
	failure := run.TurnPart{Kind: "failure", Text: "interrupted"}
	assertTranscript("alice", "one", recovered, false, []run.TurnPart{failure})
	transcript := assertTranscript("bob", "two", queuedAt, true, []run.TurnPart{partial, failure})
	if len(transcript.Turns) != 2 || transcript.Turns[1].Status != run.Queued || !transcript.Turns[1].UpdatedAt.Equal(queuedAt) {
		t.Fatalf("recovery changed the pending turn: %+v", transcript.Turns)
	}
	claimed, err := repo.Conversations.ClaimNext(ctx, queuedAt.Add(2*time.Second))
	if err != nil || claimed == nil || claimed.ID != "pending" || claimed.Status != run.Running {
		t.Fatalf("failed to resume the queue after recovery: %+v, %v", claimed, err)
	}
}

func TestHistoryReleasesSnapshotBeforeLoadingAttachments(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	seedAccounts(t, repo, "owner")
	newSession(t, repo, "owner", "session")
	now := time.Now().UTC()
	image := run.Image{Name: "photo.png", MediaType: "image/png", Data: "aW1hZ2U="}
	const historyTurns = attachmentBatchTurns + 1
	for index := range historyTurns {
		stored := run.Run{
			ID: fmt.Sprintf("saved-%d", index), OwnerID: "owner", SessionID: "session",
			Status: run.Succeeded, CreatedAt: now, UpdatedAt: now, Payload: run.Payload{Images: []run.Image{image}},
		}
		if _, err := repo.Conversations.Enqueue(ctx, stored, "", false); err != nil {
			t.Fatal(err)
		}
	}
	var loads int
	var removeTurn bool
	err = repo.read.Callback().Query().Before("gorm:query").Register("test:history-image-read", func(db *gorm.DB) {
		if db.Statement.Table == "run_attachments" {
			loads++
			if _, inTransaction := db.Statement.ConnPool.(*sql.Tx); inTransaction {
				t.Error("history still holds a read transaction while loading attachments")
			}
			if removeTurn {
				removeTurn = false
				if err := repo.db.WithContext(ctx).Delete(&row{}, "id = ?", "saved-0").Error; err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	current := run.Run{OwnerID: "owner", SessionID: "session", Sequence: historyTurns + 1}
	history, err := repo.Conversations.History(ctx, current, 0)
	if err != nil || loads != 2 || len(history) != historyTurns {
		t.Fatalf("loaded %d history turns in %d attachment batches: %v", len(history), loads, err)
	}
	for _, exchange := range history {
		if !slices.Equal(exchange.Images, []run.Image{image}) {
			t.Fatalf("expected the original historical image: %+v", exchange.Images)
		}
	}
	removeTurn = true
	if history, err := repo.Conversations.History(ctx, current, 0); err == nil || len(history) != 0 {
		t.Fatalf("returned incomplete history after the turn was deleted: %+v, %v", history, err)
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
	payload := run.Payload{Images: []run.Image{{Name: "photo.png", MediaType: "image/png"}}}
	rows := []row{
		{ID: "old", OwnerID: "owner", SessionID: "session", Sequence: 1, Status: run.Succeeded, Prompt: "old"},
		{ID: "compact", OwnerID: "owner", SessionID: "session", Sequence: 2, Status: run.Running, Prompt: "compacted", Payload: payload},
		{ID: "failed", OwnerID: "owner", SessionID: "session", Sequence: 3, Status: run.Failed, CompactedContext: checkpoint},
		{ID: "retry", OwnerID: "owner", SessionID: "session", Sequence: 4, RetryOf: "failed", Status: run.Succeeded, Prompt: "retry", Payload: payload},
		{ID: "reverted", OwnerID: "owner", SessionID: "session", Sequence: 5, Status: run.Succeeded, RevertedAt: &reverted, CompactedContext: checkpoint},
		{ID: "other", OwnerID: "other", SessionID: "other-session", Sequence: 5, Status: run.Succeeded, CompactedContext: checkpoint},
		{ID: "empty", OwnerID: "owner", SessionID: "session", Sequence: 6, Status: run.Succeeded, Prompt: "later", CompactedContext: []byte{}},
	}
	if err := repo.db.WithContext(ctx).Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"compact", "retry"} {
		if err := repo.db.Create(&runAttachmentRow{TurnID: id, Position: 0, Content: []byte("photo")}).Error; err != nil {
			t.Fatal(err)
		}
	}
	result := run.Result{CompactedContext: checkpoint, AgentMessages: []byte(`[{"role":"assistant","content":"original"}]`)}
	if err := repo.Conversations.Complete(ctx, "compact", run.Completion{Result: result, Status: run.Succeeded, UpdatedAt: reverted}); err != nil {
		t.Fatal(err)
	}
	history, err := repo.Conversations.History(ctx, run.Run{OwnerID: "owner", SessionID: "session", Sequence: 7}, 0)
	if err != nil || len(history) != 3 || history[0].Prompt != "compacted" ||
		!slices.Equal(history[0].CompactedContext, checkpoint) || history[1].Prompt != "retry" || history[2].Prompt != "later" {
		t.Fatalf("expected the current branch's checkpoint and later messages: %+v, %v", history, err)
	}
	if len(history[0].Images) != 0 || len(history[0].AgentMessages) != 0 {
		t.Fatal("loading a checkpoint fetched content already replaced by that checkpoint")
	}
	if len(history[1].Images) != 1 || history[1].Images[0].Data != "cGhvdG8=" {
		t.Fatalf("the later turn did not replay its original attachment: %+v", history[1].Images)
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
	if err := repo.Accounts.CreatePasswordUser(ctx, owner, []byte("test-digest")); err != nil {
		t.Fatal(err)
	}

	if claimed, err := repo.Conversations.ClaimNext(ctx, time.Now().UTC()); err != nil || claimed != nil {
		t.Fatalf("unowned run should stay off the queue: %+v, %v", claimed, err)
	}
	if list, err := repo.Conversations.SessionListSnapshot(ctx, owner.ID, nil, 10); err != nil || len(list.Sessions) != 0 {
		t.Fatalf("unowned run should stay private: %+v, %v", list.Sessions, err)
	}
	if count, err := repo.Conversations.AdoptLegacy(ctx, owner.ID); err != nil || count != 2 {
		t.Fatalf("adopt legacy runs: %d, %v", count, err)
	}
	if count, err := repo.Conversations.AdoptLegacy(ctx, owner.ID); err != nil || count != 0 {
		t.Fatalf("repeat adoption: %d, %v", count, err)
	}
	finished, err := repo.Conversations.Transcript(ctx, owner.ID, "finished", 10)
	if err != nil || finished == nil || len(finished.Turns) != 1 ||
		finished.Turns[0].Output != "original output" || finished.Turns[0].Prompt != "original prompt" {
		t.Fatalf("existing result was lost: %+v, %v", finished, err)
	}
	pending, err := repo.Conversations.Transcript(ctx, owner.ID, "pending", 10)
	if err != nil || pending == nil || len(pending.Turns) != 1 || pending.Turns[0].Status != run.Failed {
		t.Fatalf("old unfinished run should be imported as failed: %+v, %v", pending, err)
	}
	if other, err := repo.Conversations.Transcript(ctx, "other-owner", "finished", 10); err != nil || other != nil {
		t.Fatalf("migration exposed cross-account data: %+v, %v", other, err)
	}

	if err := repo.Settings.SetPolicy(ctx, owner.ID, sandbox.Shared); err != nil {
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
	if policy, err := reopened.Settings.Policy(ctx, owner.ID); err != nil || policy != sandbox.Shared {
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
	if err := old.Conversations.CreateSession(ctx, run.Session{ID: "stored", OwnerID: "owner", Title: "Stored session"}); err != nil {
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
	list, err := repo.Conversations.SessionListSnapshot(ctx, "owner", nil, 30)
	sessions := list.Sessions
	if err != nil || len(sessions) != 1 || sessions[0].ID != "stored" {
		t.Fatalf("recent sessions after upgrade: %+v, %v", sessions, err)
	}

	type planStep struct{ Detail string }
	for _, tc := range []struct {
		project *string
		index   string
	}{
		{index: "idx_sessions_recent (owner_id=?)"},
		{project: new("named-project"), index: "idx_sessions_project_recent (owner_id=? AND project_id=?)"},
		{project: new(""), index: "idx_sessions_project_recent (owner_id=? AND project_id=?)"},
	} {
		args := []any{[]run.Status{run.Queued, run.Running}, "owner"}
		filter := ""
		if tc.project != nil {
			filter = " AND project_id = ?"
			args = append(args, *tc.project)
		}
		var plan []planStep
		err := repo.db.Raw(`EXPLAIN QUERY PLAN
			SELECT agent_sessions.*, EXISTS (SELECT 1 FROM runs WHERE session_id = agent_sessions.id AND status IN ?) AS has_active
			FROM agent_sessions WHERE owner_id = ? AND archived_at IS NULL`+filter+`
			ORDER BY pinned DESC, updated_at DESC, id DESC LIMIT 30`, args...).Scan(&plan).Error
		if err != nil {
			t.Fatal(err)
		}
		for _, expected := range []string{tc.index, "COVERING INDEX idx_runs_session_status_sequence"} {
			if !slices.ContainsFunc(plan, func(step planStep) bool { return strings.Contains(step.Detail, expected) }) {
				t.Errorf("expected %s in session list query plan: %+v", expected, plan)
			}
		}
		if slices.ContainsFunc(plan, func(step planStep) bool { return strings.Contains(step.Detail, "TEMP B-TREE") }) {
			t.Errorf("expected the session list index to satisfy the sort: %+v", plan)
		}
	}
	if repo.db.Migrator().HasIndex(&row{}, "idx_runs_session_status") {
		t.Error("the superseded two-column index is still present")
	}
	var queuePartial bool
	if err := repo.db.Raw(`SELECT partial FROM pragma_index_list('runs') WHERE name = 'idx_runs_queue'`).Row().Scan(&queuePartial); err != nil {
		t.Fatal(err)
	}
	if !queuePartial {
		t.Error("the queue index includes completed turns")
	}
	var plan []planStep
	stmt := eligibleTurns(repo.db.Session(&gorm.Session{DryRun: true})).
		Order("candidate.created_at, candidate.id").Limit(1).Find(&[]struct{ ID string }{}).Statement
	err = repo.db.Raw("EXPLAIN QUERY PLAN "+stmt.SQL.String(), stmt.Vars...).Scan(&plan).Error
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"USING INDEX idx_runs_queue",
		"SEARCH earlier USING COVERING INDEX idx_runs_session_status_sequence (session_id=? AND status=? AND sequence<?)",
		"SEARCH executing USING COVERING INDEX idx_runs_executing_project",
	} {
		if !slices.ContainsFunc(plan, func(step planStep) bool { return strings.Contains(step.Detail, expected) }) {
			t.Errorf("expected %s in the queued turn query plan: %+v", expected, plan)
		}
	}
	if slices.ContainsFunc(plan, func(step planStep) bool { return strings.Contains(step.Detail, "TEMP B-TREE") }) {
		t.Errorf("expected the queue index to satisfy the full sort: %+v", plan)
	}
	var recoveryPlan []planStep
	if err := repo.db.Raw("EXPLAIN QUERY PLAN SELECT id FROM runs WHERE status = ?", run.Running).Scan(&recoveryPlan).Error; err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(recoveryPlan, func(step planStep) bool {
		return strings.Contains(step.Detail, "USING INDEX idx_runs_executing_project")
	}) {
		t.Errorf("expected restart recovery to use the running-turn index: %+v", recoveryPlan)
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
	if err := repo.Conversations.CreateSession(ctx, entry); err != nil {
		t.Fatal(err)
	}
	title, pinned := "Workbench", true
	if _, err := repo.Conversations.UpdateSession(ctx, "bob", entry.ID, run.SessionChanges{Title: &title}); !errors.Is(err, run.ErrSession) {
		t.Fatalf("cross-account rename: %v", err)
	}
	if _, err := repo.Conversations.UpdateSession(ctx, "alice", entry.ID, run.SessionChanges{Title: &title, Pinned: &pinned}); err != nil {
		t.Fatal(err)
	}
	turn := run.Run{
		ID: "turn", OwnerID: "alice", SessionID: entry.ID, Prompt: "First prompt",
		Status: run.Queued, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := repo.Conversations.Enqueue(ctx, turn, "Automatic title", false); err != nil {
		t.Fatal(err)
	}
	assertSessionFlags(t, repo, true, false)
	if err := repo.Conversations.ArchiveSession(ctx, "alice", entry.ID, now); !errors.Is(err, run.ErrActiveRun) {
		t.Fatalf("archive while active: %v", err)
	}
	if err := repo.Conversations.MarkSessionRead(ctx, "alice", entry.ID, 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Conversations.ClaimNext(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Conversations.Complete(ctx, turn.ID, run.Completion{Status: run.Succeeded, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	assertSessionFlags(t, repo, false, true)
	if err := repo.Conversations.MarkSessionRead(ctx, "bob", entry.ID, 1, ""); err != nil {
		t.Fatal(err)
	}
	assertSessionFlags(t, repo, false, true)
	if err := repo.Conversations.MarkSessionRead(ctx, "alice", entry.ID, 1, ""); err != nil {
		t.Fatal(err)
	}
	assertSessionFlags(t, repo, false, false)
	if err := repo.Conversations.ArchiveSession(ctx, "alice", entry.ID, now); err != nil {
		t.Fatal(err)
	}
	if session, err := repo.Conversations.Transcript(ctx, "alice", entry.ID, 10); err != nil || session != nil {
		t.Fatalf("archived session still accessible: %+v, %v", session, err)
	}
	var count int64
	if err := repo.db.WithContext(ctx).Model(&row{}).Where("id = ?", turn.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("archive did not retain the turn: count=%d, %v", count, err)
	}
}

func assertSessionFlags(t *testing.T, repo *Database, active, unread bool) {
	t.Helper()
	list, err := repo.Conversations.SessionListSnapshot(t.Context(), "alice", nil, 10)
	sessions := list.Sessions
	if err != nil || len(sessions) != 1 || !sessions[0].Pinned || sessions[0].Title != "Workbench" ||
		sessions[0].Active != active || sessions[0].Unread != unread {
		t.Fatalf("expected active=%v unread=%v, got %+v and %v", active, unread, sessions, err)
	}
}
