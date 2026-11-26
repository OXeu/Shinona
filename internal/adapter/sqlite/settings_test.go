package sqlite

import (
	"maps"
	"reflect"
	"testing"

	"github.com/OXeu/shinona/internal/settings"
)

func TestInstanceOverrideRevisionAndValuesCommitTogether(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	original := settings.Values{settings.MailFrom: "old-sender", settings.MailAPIKey: "old-key"}
	if saved, err := repo.SaveSettingOverrides(ctx, 0, original); err != nil || !saved {
		t.Fatalf("initial save: %t, %v", saved, err)
	}
	replacement := settings.Values{settings.MailFrom: "new-sender", settings.MailAPIKey: "new-key"}
	if saved, err := repo.SaveSettingOverrides(ctx, 0, replacement); err != nil || saved {
		t.Fatalf("stale save: %t, %v", saved, err)
	}
	if err := repo.db.Exec(`CREATE TRIGGER refuse_instance_secret BEFORE INSERT ON instance_setting_secrets
		BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if saved, err := repo.SaveSettingOverrides(ctx, 1, replacement); err == nil || saved {
		t.Fatalf("partial save: %t, %v", saved, err)
	}
	if stored, err := repo.SettingOverrides(ctx); err != nil || stored.Revision != 1 || !maps.Equal(stored.Values, original) {
		t.Fatalf("failed save advanced revision or changed settings: %v", err)
	}
	if saved, err := repo.SaveSettingOverrides(ctx, 1, nil); err != nil || !saved {
		t.Fatalf("reset: %t, %v", saved, err)
	}
	if stored, err := repo.SettingOverrides(ctx); err != nil || stored.Revision != 2 || len(stored.Values) != 0 {
		t.Fatalf("reading an empty snapshot lost its revision: %d, %v", stored.Revision, err)
	}
}

func TestSaveModelSettingsRollsBackConfigWhenSecretWriteFails(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	seedAccounts(t, repo, "alice")
	saved := settings.Values{settings.ModelID: "previous-model", settings.ModelAPIKey: "previous-key"}
	if err := repo.SaveModelSettings(ctx, "alice", saved); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.Exec(`CREATE TRIGGER refuse_secret BEFORE INSERT ON model_setting_secrets
		BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	next := settings.Values{settings.ModelID: "new-model", settings.ModelAPIKey: "new-key"}
	if err := repo.SaveModelSettings(ctx, "alice", next); err == nil {
		t.Fatal("save succeeded despite a failed secret write")
	}
	if values, err := repo.ModelSettings(ctx, "alice"); err != nil || !maps.Equal(values, saved) {
		t.Fatalf("failed save changed the existing model settings: %v", err)
	}
	if err := repo.SaveModelSettings(ctx, "alice", nil); err != nil {
		t.Fatal(err)
	}
	if values, err := repo.ModelSettings(ctx, "alice"); err != nil || len(values) != 0 {
		t.Fatalf("reset did not remove model settings and credentials: %v", err)
	}
}

func TestSaveMCPConfigRollsBackCredentialsWhenGrantDeletionFails(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	seedAccounts(t, repo, "alice")
	saved := settings.MCPConfig{{Name: "server", URL: "https://example.test/mcp",
		OAuth: &settings.MCPOAuthConfig{ClientID: "client", ClientSecret: "previous-secret"}}}
	if err := repo.SaveMCPConfig(ctx, "alice", saved, nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMCPGrant(ctx, "alice", "server", []byte("grant")); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.Exec(`CREATE TRIGGER refuse_grant_delete BEFORE DELETE ON mcp_oauth_grants
		BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	next := settings.MCPConfig{{Name: "server", URL: "https://example.test/changed", Token: "new-token"}}
	if err := repo.SaveMCPConfig(ctx, "alice", next, []string{"server"}); err == nil {
		t.Fatal("save succeeded despite a failed grant delete")
	}
	if servers, err := repo.MCPConfig(ctx, "alice"); err != nil || !reflect.DeepEqual(servers, saved) {
		t.Fatalf("failure to invalidate an OAuth grant partially updated MCP settings: %v", err)
	}
	if grant, err := repo.MCPGrant(ctx, "alice", "server"); err != nil || string(grant) != "grant" {
		t.Fatalf("failed update changed the saved OAuth grant: %v", err)
	}
	if err := repo.db.Exec("DROP TRIGGER refuse_grant_delete").Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMCPConfig(ctx, "alice", next, []string{"server"}); err != nil {
		t.Fatal(err)
	}
	if servers, err := repo.MCPConfig(ctx, "alice"); err != nil || !reflect.DeepEqual(servers, next) {
		t.Fatalf("retry did not replace the MCP server and credential: %v", err)
	}
	if grant, err := repo.MCPGrant(ctx, "alice", "server"); err != nil || grant != nil {
		t.Fatalf("retry did not invalidate the previous OAuth grant: %v", err)
	}
}
