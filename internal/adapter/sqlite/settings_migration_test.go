package sqlite

import (
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/OXeu/shinona/internal/settings"
)

func TestSettingsMigrationPreservesOverridesAndMCPAuthentication(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "v6.db")
	old := openSchemaFixture(t, filename, 6)
	seedAccounts(t, old, "alice", "bob")
	for _, row := range []any{
		&legacyInstanceSettingsRow{ID: 1, Data: []byte(`{"mailFrom":"","mailApiKey":"mail-secret","modelApiKey":"legacy-recovery","legacyVendor":"unknown-field"}`)},
		&legacyModelSettingsRow{OwnerID: "alice", Data: []byte(`{"modelApiKey":"alice-key","modelId":"","modelBaseUrl":"https://example.test/v1"}`)},
		&legacyModelSettingsRow{OwnerID: "bob", Data: []byte(`{"modelId":"bob-model","modelApiKey":""}`)},
		&legacyMCPSettingsRow{OwnerID: "alice", Data: []byte(`[
			{"name":"z-token","url":"https://example.test/token","token":"bearer"},
			{"name":"a-oauth","url":"https://example.test/oauth","oauth":{"clientId":"client","clientSecret":"oauth-secret"}},
			{"name":"discover","url":"https://example.test/discover","oauth":{}}
		]`)},
		&legacyMCPSettingsRow{OwnerID: "bob", Data: []byte(`[{"name":"z-token","url":"https://bob.test/mcp"}]`)},
		&mcpGrantRow{OwnerID: "alice", Server: "a-oauth", Grant: []byte("existing-grant")},
	} {
		if err := old.db.Create(row).Error; err != nil {
			t.Fatalf("seed %T: %v", row, err)
		}
	}
	var modelRows []legacyModelSettingsRow
	var mcpRows []legacyMCPSettingsRow
	for i := range 100 {
		owner := fmt.Sprintf("fixture-%03d", i)
		seedAccounts(t, old, owner)
		modelRows = append(modelRows, legacyModelSettingsRow{OwnerID: owner, Data: []byte(`{"modelId":"fixture-model"}`)})
		mcpRows = append(mcpRows, legacyMCPSettingsRow{OwnerID: owner, Data: []byte(`[{"name":"server","url":"https://fixture.test/mcp"}]`)})
	}
	if err := old.db.Create(&modelRows).Error; err != nil {
		t.Fatal(err)
	}
	if err := old.db.Create(&mcpRows).Error; err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	repo, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if version, err := schemaVersion(repo.db); err != nil || version != len(migrations) {
		t.Fatalf("settings migration version = %d, %v", version, err)
	}
	for _, table := range []string{"instance_settings", "model_settings", "mcp_settings"} {
		if repo.db.Migrator().HasTable(table) {
			t.Fatalf("old JSON settings table %s still exists", table)
		}
	}
	wantInstance := settings.Values{
		settings.MailFrom: "", settings.MailAPIKey: "mail-secret", settings.ModelAPIKey: "legacy-recovery", "legacyVendor": "unknown-field",
	}
	if stored, err := repo.Settings.SettingOverrides(ctx); err != nil || stored.Revision != 0 || !maps.Equal(stored.Values, wantInstance) {
		t.Fatalf("instance settings changed during migration: %v", err)
	}
	wantAlice := settings.Values{settings.ModelAPIKey: "alice-key", settings.ModelBaseURL: "https://example.test/v1", settings.ModelID: ""}
	if values, err := repo.Settings.ModelSettings(ctx, "alice"); err != nil || !maps.Equal(values, wantAlice) {
		t.Fatalf("Alice's model settings changed during migration: %v", err)
	}
	wantBob := settings.Values{settings.ModelID: "bob-model", settings.ModelAPIKey: ""}
	if values, err := repo.Settings.ModelSettings(ctx, "bob"); err != nil || !maps.Equal(values, wantBob) {
		t.Fatalf("Bob's model settings changed during migration: %v", err)
	}
	wantMCP := settings.MCPConfig{
		{Name: "z-token", URL: "https://example.test/token", Token: "bearer"},
		{Name: "a-oauth", URL: "https://example.test/oauth", OAuth: &settings.MCPOAuthConfig{ClientID: "client", ClientSecret: "oauth-secret"}},
		{Name: "discover", URL: "https://example.test/discover", OAuth: &settings.MCPOAuthConfig{}},
	}
	if servers, err := repo.Settings.MCPConfig(ctx, "alice"); err != nil || !reflect.DeepEqual(servers, wantMCP) {
		t.Fatalf("MCP order or authentication changed during migration: %v", err)
	}
	if servers, err := repo.Settings.MCPConfig(ctx, "bob"); err != nil || len(servers) != 1 || servers[0].Token != "" || servers[0].OAuth != nil {
		t.Fatalf("MCP migration mixed accounts with the same server name: %v", err)
	}
	if grant, err := repo.Settings.MCPGrant(ctx, "alice", "a-oauth"); err != nil || string(grant) != "existing-grant" {
		t.Fatalf("migration changed an active grant: %v", err)
	}
	if values, err := repo.Settings.ModelSettings(ctx, "fixture-099"); err != nil || values[settings.ModelID] != "fixture-model" {
		t.Fatalf("migration missed model settings beyond the first batch: %v", err)
	}
	if servers, err := repo.Settings.MCPConfig(ctx, "fixture-099"); err != nil || len(servers) != 1 || servers[0].Name != "server" {
		t.Fatalf("migration missed MCP settings beyond the first batch: %v", err)
	}
	for query, want := range map[string]int{
		"SELECT count(*) FROM instance_setting_values WHERE key = 'mailFrom'":                    1,
		"SELECT count(*) FROM instance_setting_values":                                           1,
		"SELECT count(*) FROM instance_setting_secrets":                                          3,
		"SELECT count(*) FROM model_setting_values WHERE key = 'modelApiKey'":                    0,
		"SELECT count(*) FROM model_setting_secrets":                                             2,
		"SELECT count(*) FROM mcp_server_secrets WHERE owner_id = 'alice' AND name = 'a-oauth'":  1,
		"SELECT count(*) FROM mcp_server_secrets WHERE owner_id = 'alice' AND name = 'discover'": 0,
		"SELECT count(*) FROM mcp_servers WHERE owner_id = 'alice' AND oauth_client_id = ''":     1,
	} {
		var count int
		if err := repo.db.Raw(query).Row().Scan(&count); err != nil || count != want {
			t.Errorf("%s = %d, want %d: %v", query, count, want, err)
		}
	}
}

func TestSettingsMigrationRollsBackMalformedLegacyMCP(t *testing.T) {
	ctx := t.Context()
	repo := openSchemaFixture(t, filepath.Join(t.TempDir(), "v6.db"), 6)
	seedAccounts(t, repo, "owner")
	if err := repo.db.Create(&legacyModelSettingsRow{OwnerID: "owner", Data: []byte(`{"modelApiKey":"preserve"}`)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.db.Create(&legacyMCPSettingsRow{OwnerID: "owner", Data: []byte(`{"token":"malformed-secret"}`)}).Error; err != nil {
		t.Fatal(err)
	}
	err := migrate(repo.db.WithContext(ctx))
	if err == nil || !strings.Contains(err.Error(), "decode legacy MCP settings for account") || strings.Contains(err.Error(), "malformed-secret") {
		t.Fatalf("migration should report malformed settings without exposing the credential: %v", err)
	}
	if version, err := schemaVersion(repo.db); err != nil || version != 6 || repo.db.Migrator().HasTable("model_setting_secrets") {
		t.Fatalf("failed migration partially changed the schema: %d, %v", version, err)
	}
	if err := repo.db.Model(&legacyMCPSettingsRow{}).Where("owner_id = ?", "owner").Update("data", []byte("[]")).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrate(repo.db.WithContext(ctx)); err != nil {
		t.Fatal(err)
	}
	if values, err := repo.Settings.ModelSettings(ctx, "owner"); err != nil || values[settings.ModelAPIKey] != "preserve" {
		t.Fatalf("a failed migration changed the legacy model key: %v", err)
	}
}
