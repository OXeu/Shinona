package sqlite

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/OXeu/shinona/internal/run"
)

func TestProjectSandboxSettingsKeepLegacyE2BProvider(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "projects.db")
	repo, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	project := run.Project{ID: "project", OwnerID: "owner", Name: "Project"}
	if err := repo.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if saved, err := repo.SaveE2BSandbox(ctx, project.OwnerID, project.ID, "fixture", "", "legacy-box", []byte("{}")); err != nil || !saved {
		t.Fatalf("save legacy sandbox: %v, %v", saved, err)
	}
	asLegacyDatabase(t, repo)
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := repo.RememberProjectSandboxProvider(ctx, project.OwnerID, project.ID, "ssh"); err != nil {
		t.Fatal(err)
	}
	config, err := repo.ProjectSandboxConfig(ctx, project.OwnerID, project.ID)
	if err != nil || config == nil || *config != (run.ProjectSandboxConfig{Provider: "e2b"}) {
		t.Fatalf("legacy project should keep E2B and inherit its settings: %+v, %v", config, err)
	}
	if err := repo.SaveProjectSandboxConfig(ctx, "other-owner", project.ID, run.ProjectSandboxConfig{Provider: "ssh"}); !errors.Is(err, run.ErrProject) {
		t.Fatalf("cross-account settings write: %v", err)
	}
}
