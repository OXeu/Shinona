package sqlite

import (
	"errors"
	"path/filepath"
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

	if err := repo.SetWorkspaceRoot(ctx, "owner", "session", "", "packages/api"); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.WorkspaceRoot(ctx, "owner", "session"); err != nil || root != "packages/api" {
		t.Fatalf("saved root: %q, %v", root, err)
	}
	// Setting again replaces the previous directory.
	if err := repo.SetWorkspaceRoot(ctx, "owner", "session", "", "packages/web"); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.WorkspaceRoot(ctx, "owner", "session"); err != nil || root != "packages/web" {
		t.Fatalf("replaced root: %q, %v", root, err)
	}
	// An empty root clears the preference.
	if err := repo.SetWorkspaceRoot(ctx, "owner", "session", "", ""); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.WorkspaceRoot(ctx, "owner", "session"); err != nil || root != "" {
		t.Fatalf("cleared root: %q, %v", root, err)
	}
}

func TestWorkspaceRootIsSharedOnlyWithinTheOwnersProject(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	newSession(t, repo, "owner", "session")
	newSession(t, repo, "owner", "other")
	newSession(t, repo, "intruder", "intruder-session")
	if err := repo.CreateProject(ctx, run.Project{ID: "project", OwnerID: "owner", Name: "Project"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, id := range []string{"project-session", "project-sibling"} {
		if err := repo.CreateSession(ctx, run.Session{ID: id, OwnerID: "owner", ProjectID: "project", Title: id, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	if err := repo.SetWorkspaceRoot(ctx, "owner", "session", "", "packages/api"); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.WorkspaceRoot(ctx, "owner", "other"); err != nil || root != "packages/api" {
		t.Fatalf("sibling session root: %q, %v", root, err)
	}
	if err := repo.SetWorkspaceRoot(ctx, "owner", "project-session", "project", "src"); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.WorkspaceRoot(ctx, "owner", "project-sibling"); err != nil || root != "src" {
		t.Fatalf("named project sibling root: %q, %v", root, err)
	}
	if root, err := repo.WorkspaceRoot(ctx, "intruder", "intruder-session"); err != nil || root != "" {
		t.Fatalf("another account's default root: %q, %v", root, err)
	}
	// Another account cannot read or write the session.
	if _, err := repo.WorkspaceRoot(ctx, "intruder", "session"); !errors.Is(err, run.ErrSession) {
		t.Fatalf("cross-account read: %v", err)
	}
	if err := repo.SetWorkspaceRoot(ctx, "intruder", "session", "", "elsewhere"); !errors.Is(err, run.ErrSession) {
		t.Fatalf("cross-account write: %v", err)
	}
	if err := repo.RemoveProject(ctx, "owner", "project"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetWorkspaceRoot(ctx, "owner", "project-session", "project", "removed"); !errors.Is(err, run.ErrWorkspaceBusy) {
		t.Fatalf("write from a stale project: %v", err)
	}
	if root, err := repo.WorkspaceRoot(ctx, "owner", "project-session"); err != nil || root != "packages/api" {
		t.Fatalf("root of a session moved to default: %q, %v", root, err)
	}
	var count int64
	if err := repo.db.Model(&workspaceRootRow{}).Where("project_id = ?", "project").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("root rows after project deletion: %d, %v", count, err)
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
	if err := repo.SetWorkspaceRoot(ctx, "owner", "missing", "", "api"); !errors.Is(err, run.ErrSession) {
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

func TestWorkspaceRootMigratesLegacyPreferencesOnce(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "workspace.db")
	open := func() *Repository {
		t.Helper()
		repo, err := Open(ctx, filename)
		if err != nil {
			t.Fatal(err)
		}
		return repo
	}
	repo := open()
	if err := repo.db.Migrator().DropTable(&workspaceRootRow{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, entry := range []struct{ session, root string }{
		{"a", "src"}, {"b", "packages/api"}, {"archived", "old"},
	} {
		at := now.Add(time.Duration(i) * time.Second)
		if err := repo.CreateSession(ctx, run.Session{ID: entry.session, OwnerID: "owner", Title: entry.session, CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := repo.db.Create(&legacyWorkspaceRootRow{OwnerID: "owner", SessionID: entry.session, Root: entry.root}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.ArchiveSession(ctx, "owner", "archived", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo = open()
	if root, err := repo.WorkspaceRoot(ctx, "owner", "a"); err != nil || root != "packages/api" {
		t.Fatalf("imported working directory: %q, %v", root, err)
	}
	if err := repo.SetWorkspaceRoot(ctx, "owner", "b", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo = open()
	t.Cleanup(func() { _ = repo.Close() })
	if root, err := repo.WorkspaceRoot(ctx, "owner", "a"); err != nil || root != "" {
		t.Fatalf("reopened project restored an old choice: %q, %v", root, err)
	}
}
