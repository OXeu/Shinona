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

func TestRemoveProjectKeepsPendingTurnsOutOfTheDefaultWorkspace(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	const owner, project, session = "owner", "project", "project-session"
	seedAccounts(t, repo, owner)
	newSession(t, repo, owner, "default-session")
	if err := repo.Projects.CreateProject(ctx, run.Project{ID: project, OwnerID: owner, Name: "Fixture"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := repo.Conversations.CreateSession(ctx, run.Session{
		ID: session, OwnerID: owner, ProjectID: project, Title: "Fixture", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for index, item := range []run.Run{
		{ID: "claimed", OwnerID: owner, SessionID: session, ProjectID: project, Prompt: "Claimed prompt"},
		{ID: "pending", OwnerID: owner, SessionID: session, ProjectID: project, Prompt: "Pending prompt"},
		{ID: "default-turn", OwnerID: owner, SessionID: "default-session"},
	} {
		item.Status = run.Queued
		item.CreatedAt = now.Add(time.Duration(index) * time.Millisecond)
		item.UpdatedAt = item.CreatedAt
		if _, err := repo.Conversations.Enqueue(ctx, item, "", true); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := repo.Conversations.ClaimNext(ctx, now)
	if err != nil || claimed == nil || claimed.ID != "claimed" || claimed.ProjectID != project || claimed.StartedAt == nil {
		t.Fatalf("claim did not capture its project: %+v, %v", claimed, err)
	}
	if err := repo.Projects.RemoveProject(ctx, owner, project); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Conversations.Enqueue(ctx, run.Run{
		ID: "late", OwnerID: owner, SessionID: session, ProjectID: project, Status: run.Queued,
	}, "", true); !errors.Is(err, run.ErrWorkspaceBusy) {
		t.Fatalf("late submission to the old workspace: %v", err)
	}
	if err := repo.Conversations.Complete(ctx, claimed.ID, run.Completion{
		Status: run.Stopped, UpdatedAt: now, Unstarted: true,
	}); err != nil {
		t.Fatal(err)
	}
	next, err := repo.Conversations.ClaimNext(ctx, now)
	if err != nil || next == nil || next.ID != "default-turn" || next.ProjectID != "" {
		t.Fatalf("project removal affected default workspace scheduling: %+v, %v", next, err)
	}
	page, err := repo.Conversations.TranscriptPage(ctx, owner, session, 0)
	if err != nil || page == nil || page.Session.ProjectID != "" || len(page.Turns) != 2 {
		t.Fatalf("the removed project's conversation was not preserved: %+v, %v", page, err)
	}
	for index, prompt := range []string{"Claimed prompt", "Pending prompt"} {
		turn := page.Turns[index]
		if turn.Status != run.Stopped || turn.StartedAt != nil || turn.Prompt != prompt {
			t.Errorf("turn %d was not preserved as an unstarted stop: %+v", index, turn)
		}
	}
	history, err := repo.Conversations.History(ctx, run.Run{OwnerID: owner, SessionID: session, Sequence: 3}, 0)
	if err != nil || len(history) != 0 {
		t.Fatalf("unstarted turns reached the next model context: %+v, %v", history, err)
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

	selection, err := repo.Projects.WorkspaceSelection(ctx, "owner", "session")
	if err != nil {
		t.Fatal(err)
	}
	if selection != (run.WorkspaceSelection{}) {
		t.Fatalf("default selection: %+v", selection)
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
	if selection, err := repo.Projects.WorkspaceSelection(ctx, "owner", "session"); err != nil || selection.Root != "packages/api" {
		t.Fatalf("saved root: %q, %v", selection.Root, err)
	}
	// Setting again replaces the previous directory.
	if err := repo.Projects.SetWorkspaceRoot(ctx, "owner", "session", "", "packages/web"); err != nil {
		t.Fatal(err)
	}
	if selection, err := repo.Projects.WorkspaceSelection(ctx, "owner", "session"); err != nil || selection.Root != "packages/web" {
		t.Fatalf("replaced root: %q, %v", selection.Root, err)
	}
	// An empty root clears the preference.
	if err := repo.Projects.SetWorkspaceRoot(ctx, "owner", "session", "", ""); err != nil {
		t.Fatal(err)
	}
	if selection, err := repo.Projects.WorkspaceSelection(ctx, "owner", "session"); err != nil || selection.Root != "" {
		t.Fatalf("cleared root: %q, %v", selection.Root, err)
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
	if selection, err := repo.Projects.WorkspaceSelection(ctx, "owner", "other"); err != nil || selection.Root != "packages/api" {
		t.Fatalf("sibling session root: %q, %v", selection.Root, err)
	}
	if err := repo.Projects.SetWorkspaceRoot(ctx, "owner", "project-session", "project", "src"); err != nil {
		t.Fatal(err)
	}
	if selection, err := repo.Projects.WorkspaceSelection(ctx, "owner", "project-sibling"); err != nil || selection != (run.WorkspaceSelection{ProjectID: "project", Root: "src"}) {
		t.Fatalf("named project sibling selection: %+v, %v", selection, err)
	}
	if selection, err := repo.Projects.WorkspaceSelection(ctx, "intruder", "intruder-session"); err != nil || selection.Root != "" {
		t.Fatalf("another account's default root: %q, %v", selection.Root, err)
	}
	// Another account cannot read or write the session.
	if _, err := repo.Projects.WorkspaceSelection(ctx, "intruder", "session"); !errors.Is(err, run.ErrSession) {
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
	if err := repo.Projects.RemoveProject(ctx, "owner", "project"); !errors.Is(err, run.ErrProjectCleanupRequired) {
		t.Fatalf("remove project before sandbox cleanup: %v", err)
	}
	if selection, err := repo.Projects.WorkspaceSelection(ctx, "owner", "project-session"); err != nil || selection != (run.WorkspaceSelection{ProjectID: "project", Root: "src"}) {
		t.Fatalf("rejected removal changed the session workspace: %+v, %v", selection, err)
	}
	if err := repo.E2B.DeleteE2BSandbox(ctx, "owner", "project", "fixture", "box"); err != nil {
		t.Fatal(err)
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
	if selection, err := repo.Projects.WorkspaceSelection(ctx, "owner", "project-session"); err != nil || selection.Root != "packages/api" {
		t.Fatalf("root of a session moved to default: %q, %v", selection.Root, err)
	}
	var count int64
	if err := repo.db.Model(&workspaceRootRow{}).Where("project_id = ?", "project").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("root rows after project deletion: %d, %v", count, err)
	}
	if selection, err := repo.Projects.WorkspaceSelection(ctx, "owner", "session"); err != nil || selection.Root != "packages/api" {
		t.Fatalf("root after rejected write: %q, %v", selection.Root, err)
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

	if _, err := repo.Projects.WorkspaceSelection(ctx, "owner", "missing"); !errors.Is(err, run.ErrSession) {
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
	if selection, err := repo.Projects.WorkspaceSelection(ctx, "owner", "a"); err != nil || selection.Root != "packages/api" {
		t.Fatalf("imported working directory: %q, %v", selection.Root, err)
	}
	if err := repo.Projects.SetWorkspaceRoot(ctx, "owner", "b", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo = open()
	t.Cleanup(func() { _ = repo.Close() })
	if selection, err := repo.Projects.WorkspaceSelection(ctx, "owner", "a"); err != nil || selection.Root != "" {
		t.Fatalf("reopened project restored an old choice: %q, %v", selection.Root, err)
	}
}
