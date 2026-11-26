package sqlite

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func newSession(t *testing.T, repo *Database, owner, id string) {
	t.Helper()
	if err := repo.Conversations.CreateSession(t.Context(), run.Session{
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
	seedAccounts(t, repo, "owner")
	newSession(t, repo, "owner", "session")

	root, err := repo.Projects.WorkspaceRoot(ctx, "owner", "session")
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
	seedAccounts(t, repo, "owner")
	newSession(t, repo, "owner", "session")

	if err := repo.Projects.SetWorkspaceRoot(ctx, "owner", "session", "", "packages/api"); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.Projects.WorkspaceRoot(ctx, "owner", "session"); err != nil || root != "packages/api" {
		t.Fatalf("saved root: %q, %v", root, err)
	}
	// Setting again replaces the previous directory.
	if err := repo.Projects.SetWorkspaceRoot(ctx, "owner", "session", "", "packages/web"); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.Projects.WorkspaceRoot(ctx, "owner", "session"); err != nil || root != "packages/web" {
		t.Fatalf("replaced root: %q, %v", root, err)
	}
	// An empty root clears the preference.
	if err := repo.Projects.SetWorkspaceRoot(ctx, "owner", "session", "", ""); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.Projects.WorkspaceRoot(ctx, "owner", "session"); err != nil || root != "" {
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
	seedAccounts(t, repo, "owner", "intruder")
	newSession(t, repo, "owner", "session")
	newSession(t, repo, "owner", "other")
	newSession(t, repo, "intruder", "intruder-session")
	if err := repo.Projects.CreateProject(ctx, run.Project{ID: "project", OwnerID: "owner", Name: "Project"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, id := range []string{"project-session", "project-sibling", "archived-project-session"} {
		if err := repo.Conversations.CreateSession(ctx, run.Session{ID: id, OwnerID: "owner", ProjectID: "project", Title: id, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Conversations.ArchiveSession(ctx, "owner", "archived-project-session", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Conversations.CreateSession(ctx, run.Session{ID: "wrong-owner", OwnerID: "intruder", ProjectID: "project"}); err == nil {
		t.Fatal("another account's project accepted a conversation")
	}

	if err := repo.Projects.SetWorkspaceRoot(ctx, "owner", "session", "", "packages/api"); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.Projects.WorkspaceRoot(ctx, "owner", "other"); err != nil || root != "packages/api" {
		t.Fatalf("sibling session root: %q, %v", root, err)
	}
	if err := repo.Projects.SetWorkspaceRoot(ctx, "owner", "project-session", "project", "src"); err != nil {
		t.Fatal(err)
	}
	if root, err := repo.Projects.WorkspaceRoot(ctx, "owner", "project-sibling"); err != nil || root != "src" {
		t.Fatalf("named project sibling root: %q, %v", root, err)
	}
	if root, err := repo.Projects.WorkspaceRoot(ctx, "intruder", "intruder-session"); err != nil || root != "" {
		t.Fatalf("another account's default root: %q, %v", root, err)
	}
	// Another account cannot read or write the session.
	if _, err := repo.Projects.WorkspaceRoot(ctx, "intruder", "session"); !errors.Is(err, run.ErrSession) {
		t.Fatalf("cross-account read: %v", err)
	}
	if err := repo.Projects.SetWorkspaceRoot(ctx, "intruder", "session", "", "elsewhere"); !errors.Is(err, run.ErrSession) {
		t.Fatalf("cross-account write: %v", err)
	}
	if err := repo.Projects.SaveProjectSandboxConfig(ctx, "owner", "project", run.ProjectSandboxConfig{Provider: "e2b"}); err != nil {
		t.Fatal(err)
	}
	if saved, err := repo.E2B.SaveE2BSandbox(ctx, "owner", "project", "fixture", "", "box", []byte("{}")); err != nil || !saved {
		t.Fatalf("fixture binding: %t, %v", saved, err)
	}
	if err := repo.Projects.RemoveProject(ctx, "owner", "project"); err != nil {
		t.Fatal(err)
	}
	var archived sessionRow
	if err := repo.db.Take(&archived, "id = ?", "archived-project-session").Error; err != nil || archived.ProjectID != "" || archived.ArchivedAt == nil {
		t.Fatalf("archived conversation was lost or reassigned incorrectly: %+v, %v", archived, err)
	}
	for _, table := range []string{"project_sandbox_settings", "project_workspace_roots", "e2b_project_workspaces"} {
		var count int64
		if err := repo.db.Table(table).Where("owner_id = ? AND project_id = ?", "owner", "project").Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("%s has %d rows after project deletion: %v", table, count, err)
		}
	}
	if _, err := repo.E2B.SaveE2BSandbox(ctx, "owner", "project", "fixture", "", "late-box", []byte("{}")); err == nil {
		t.Fatal("late provisioning recreated the binding for a deleted project")
	}
	if err := repo.Projects.SetWorkspaceRoot(ctx, "owner", "project-session", "project", "removed"); !errors.Is(err, run.ErrWorkspaceBusy) {
		t.Fatalf("write from a stale project: %v", err)
	}
	if root, err := repo.Projects.WorkspaceRoot(ctx, "owner", "project-session"); err != nil || root != "packages/api" {
		t.Fatalf("root of a session moved to default: %q, %v", root, err)
	}
	var count int64
	if err := repo.db.Model(&workspaceRootRow{}).Where("project_id = ?", "project").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("root rows after project deletion: %d, %v", count, err)
	}
	if root, err := repo.Projects.WorkspaceRoot(ctx, "owner", "session"); err != nil || root != "packages/api" {
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
	seedAccounts(t, repo, "owner")
	newSession(t, repo, "owner", "session")

	if _, err := repo.Projects.WorkspaceRoot(ctx, "owner", "missing"); !errors.Is(err, run.ErrSession) {
		t.Fatalf("missing session read: %v", err)
	}
	if err := repo.Projects.SetWorkspaceRoot(ctx, "owner", "missing", "", "api"); !errors.Is(err, run.ErrSession) {
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
	open := func() *Database {
		t.Helper()
		repo, err := Open(ctx, filename)
		if err != nil {
			t.Fatal(err)
		}
		return repo
	}
	repo := openSchemaFixture(t, filename, 1)
	seedAccounts(t, repo, "owner")
	if err := repo.db.Migrator().DropTable(&workspaceRootRow{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, entry := range []struct{ session, root string }{
		{"a", "src"}, {"b", "packages/api"}, {"archived", "old"},
	} {
		at := now.Add(time.Duration(i) * time.Second)
		if err := repo.Conversations.CreateSession(ctx, run.Session{ID: entry.session, OwnerID: "owner", Title: entry.session, CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := repo.db.Create(&legacyWorkspaceRootRow{OwnerID: "owner", SessionID: entry.session, Root: entry.root}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Conversations.ArchiveSession(ctx, "owner", "archived", now); err != nil {
		t.Fatal(err)
	}
	markUnversioned(t, repo)
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo = open()
	if root, err := repo.Projects.WorkspaceRoot(ctx, "owner", "a"); err != nil || root != "packages/api" {
		t.Fatalf("imported working directory: %q, %v", root, err)
	}
	if err := repo.Projects.SetWorkspaceRoot(ctx, "owner", "b", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo = open()
	t.Cleanup(func() { _ = repo.Close() })
	if root, err := repo.Projects.WorkspaceRoot(ctx, "owner", "a"); err != nil || root != "" {
		t.Fatalf("reopened project restored an old choice: %q, %v", root, err)
	}
}
