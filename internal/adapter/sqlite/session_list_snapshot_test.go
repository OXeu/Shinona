package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/run"
)

func TestSessionListSnapshotKeepsProjectsAndSessionsOnOneRevision(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "shinona.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	const owner, project, session = "owner", "project", "session"
	seedAccounts(t, repo, owner)
	if err := repo.Projects.CreateProject(ctx, run.Project{OwnerID: owner, ID: project, Name: "old project"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Conversations.CreateSession(ctx, run.Session{OwnerID: owner, ID: session, ProjectID: project, Title: "old session"}); err != nil {
		t.Fatal(err)
	}
	revision, err := repo.Conversations.SessionListRevision(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}

	updated := false
	err = repo.read.Callback().Query().After("gorm:query").Register("test:update_session_list", func(query *gorm.DB) {
		if query.Statement.Table != "projects" || updated {
			return
		}
		updated = true
		// Commit both updates after the project query and before the session query.
		err := repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&projectRow{}).Where("id = ?", project).UpdateColumn("name", "new project").Error; err != nil {
				return err
			}
			return tx.Model(&sessionRow{}).Where("id = ?", session).UpdateColumn("title", "new session").Error
		})
		if err != nil {
			_ = query.AddError(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	projectID := project
	assertSnapshot := func(projectName, sessionTitle string) run.SessionListSnapshot {
		t.Helper()
		snapshot, err := repo.Conversations.SessionListSnapshot(ctx, owner, &projectID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Projects) != 1 || snapshot.Projects[0].Name != projectName ||
			len(snapshot.Sessions) != 1 || snapshot.Sessions[0].Title != sessionTitle {
			t.Fatalf("expected %q and %q, got %+v", projectName, sessionTitle, snapshot)
		}
		return snapshot
	}
	if snapshot := assertSnapshot("old project", "old session"); !updated || snapshot.Revision != revision {
		t.Fatalf("expected revision %d before the concurrent update, got %+v (updated: %t)", revision, snapshot, updated)
	}
	if snapshot := assertSnapshot("new project", "new session"); snapshot.Revision <= revision {
		t.Fatalf("expected revision to advance past %d, got %+v", revision, snapshot)
	}
}
