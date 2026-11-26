package sqlite

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

func TestProjectSandboxSettingsKeepLegacyE2BProvider(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "projects.db")
	repo := openSchemaFixture(t, filename, 1)
	seedAccounts(t, repo, "owner")
	project := run.Project{ID: "project", OwnerID: "owner", Name: "Project"}
	if err := repo.Projects.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if saved, err := repo.E2B.SaveE2BSandbox(ctx, project.OwnerID, project.ID, "fixture", "", "legacy-box", []byte("{}")); err != nil || !saved {
		t.Fatalf("save legacy sandbox: %v, %v", saved, err)
	}
	markUnversioned(t, repo)
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := repo.Projects.RememberProjectSandboxProvider(ctx, project.OwnerID, project.ID, "ssh"); err != nil {
		t.Fatal(err)
	}
	config, err := repo.Projects.ProjectSandboxConfig(ctx, project.OwnerID, project.ID)
	if err != nil || config == nil || *config != (run.ProjectSandboxConfig{Provider: "e2b"}) {
		t.Fatalf("legacy project should keep E2B and inherit its settings: %+v, %v", config, err)
	}
	if err := repo.Projects.SaveProjectSandboxConfig(ctx, "other-owner", project.ID, run.ProjectSandboxConfig{Provider: "ssh"}); !errors.Is(err, run.ErrProject) {
		t.Fatalf("cross-account settings write: %v", err)
	}
}

func TestMigratesProjectSSHBindingsAndEnforcesOwnership(t *testing.T) {
	const previousVersion = 10
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "ssh-bindings.db")
	old := openSchemaFixture(t, filename, previousVersion)
	seedAccounts(t, old, "alice", "bob")
	server := settings.SSHServer{SSHServerInfo: settings.SSHServerInfo{ID: "host", Name: "Build host"}}
	if err := old.Settings.SaveSSHServer(ctx, "alice", server, true); err != nil {
		t.Fatal(err)
	}
	personal := settings.SSHProviderName(server.ID)
	for _, owner := range []string{"alice", "bob"} {
		if err := old.db.Exec(`INSERT INTO project_sandbox_settings (owner_id, project_id, provider, template)
			VALUES (?, '', ?, 'legacy-image')`, owner, personal).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	for _, owner := range []string{"alice", "bob"} {
		config, err := repo.Projects.ProjectSandboxConfig(ctx, owner, "")
		if err != nil || config == nil || config.Provider != personal || config.Template != "legacy-image" {
			t.Fatalf("legacy sandbox config for %s = %+v, %v", owner, config, err)
		}
		var record projectSandboxRow
		if err := repo.db.Take(&record, "owner_id = ? AND project_id = ''", owner).Error; err != nil {
			t.Fatal(err)
		}
		if owner == "alice" && (record.SSHServerID == nil || *record.SSHServerID != server.ID || record.Provider != sandbox.ProviderSSH) {
			t.Fatalf("valid assignment was not normalized: %+v", record)
		}
		if owner == "bob" && record.SSHServerID != nil {
			t.Fatalf("migration linked the other account's server: %+v", record)
		}
	}
	if err := repo.db.Exec(`UPDATE project_sandbox_settings SET provider = ?, ssh_server_id = ? WHERE owner_id = 'bob'`,
		sandbox.ProviderSSH, server.ID).Error; err == nil {
		t.Fatal("SQLite accepted a cross-account SSH assignment")
	}
	if err := repo.Projects.SaveProjectSandboxConfig(ctx, "bob", "", run.ProjectSandboxConfig{Provider: personal}); !errors.Is(err, settings.ErrSSHServer) {
		t.Fatalf("cross-account project configuration returned %v", err)
	}
	if err := repo.Settings.RemoveSSHServer(ctx, "alice", server.ID); !errors.Is(err, settings.ErrSSHServerInUse) {
		t.Fatalf("removing an assigned server returned %v", err)
	}
	e2b := run.ProjectSandboxConfig{Provider: sandbox.ProviderE2B, Template: "custom-template", TimeoutSeconds: 45, OnTimeout: sandbox.TimeoutKill}
	if err := repo.Projects.SaveProjectSandboxConfig(ctx, "alice", "", e2b); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.Projects.ProjectSandboxConfig(ctx, "alice", ""); err != nil || got == nil || *got != e2b {
		t.Fatalf("switching to E2B returned %+v, %v", got, err)
	}
	if err := repo.Settings.RemoveSSHServer(ctx, "alice", server.ID); err != nil {
		t.Fatalf("switched project still kept the SSH binding: %v", err)
	}
	if err := repo.Settings.SaveSSHServer(ctx, "alice", server, true); err != nil {
		t.Fatal(err)
	}
	if err := repo.Projects.SaveProjectSandboxConfig(ctx, "alice", "", run.ProjectSandboxConfig{Provider: personal}); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.Delete(&userRow{}, "id = ?", "alice").Error; err != nil {
		t.Fatalf("SSH binding blocked account deletion: %v", err)
	}
	if err := checkForeignKeys(repo.db); err != nil {
		t.Fatal(err)
	}
}
