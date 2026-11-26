package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func newSession(t *testing.T, repo *Repository, owner, id string) {
	t.Helper()
	if err := repo.CreateSession(t.Context(), run.Session{
		ID: id, OwnerID: owner, Title: "Session", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceRootDefaultsToTheProjectWorkspace(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	newSession(t, repo, "owner", "session")

	root, err := repo.WorkspaceRoot(ctx, "owner", "session")
	if err != nil {
		t.Fatal(err)
	}
	if root != "" {
		t.Fatalf("default root: %q", root)
	}
}

func TestWorkspaceRootRoundTripsAndResets(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	newSession(t, repo, "owner", "session")

	if err := repo.SetWorkspaceRoot(ctx, "owner", "session", "packages/api"); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.WorkspaceRoot(ctx, "owner", "session"); err != nil || root != "packages/api" {
		t.Fatalf("saved root: %q, %v", root, err)
	}
	// Setting again replaces the previous directory.
	if err := repo.SetWorkspaceRoot(ctx, "owner", "session", "packages/web"); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.WorkspaceRoot(ctx, "owner", "session"); err != nil || root != "packages/web" {
		t.Fatalf("replaced root: %q, %v", root, err)
	}
	// An empty root clears the preference.
	if err := repo.SetWorkspaceRoot(ctx, "owner", "session", ""); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.WorkspaceRoot(ctx, "owner", "session"); err != nil || root != "" {
		t.Fatalf("cleared root: %q, %v", root, err)
	}
}

func TestWorkspaceRootIsScopedToTheOwnerAndSession(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	newSession(t, repo, "owner", "session")
	newSession(t, repo, "owner", "other")
	newSession(t, repo, "intruder", "intruder-session")

	if err := repo.SetWorkspaceRoot(ctx, "owner", "session", "packages/api"); err != nil {
		t.Fatal(err)
	}
	// Another session of the same account keeps the default.
	if root, err := repo.WorkspaceRoot(ctx, "owner", "other"); err != nil || root != "" {
		t.Fatalf("sibling session root: %q, %v", root, err)
	}
	// Another account cannot read or write the session.
	if _, err := repo.WorkspaceRoot(ctx, "intruder", "session"); !errors.Is(err, run.ErrSession) {
		t.Fatalf("cross-account read: %v", err)
	}
	if err := repo.SetWorkspaceRoot(ctx, "intruder", "session", "elsewhere"); !errors.Is(err, run.ErrSession) {
		t.Fatalf("cross-account write: %v", err)
	}
	if root, err := repo.WorkspaceRoot(ctx, "owner", "session"); err != nil || root != "packages/api" {
		t.Fatalf("root after rejected write: %q, %v", root, err)
	}
}

func TestWorkspaceRootRejectsUnknownSessions(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	newSession(t, repo, "owner", "session")

	if _, err := repo.WorkspaceRoot(ctx, "owner", "missing"); !errors.Is(err, run.ErrSession) {
		t.Fatalf("missing session read: %v", err)
	}
	if err := repo.SetWorkspaceRoot(ctx, "owner", "missing", "api"); !errors.Is(err, run.ErrSession) {
		t.Fatalf("missing session write: %v", err)
	}
	// A missing session must not leave a row behind.
	var count int64
	if err := repo.db.Model(&workspaceRootRow{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale rows: %d", count)
	}
}
