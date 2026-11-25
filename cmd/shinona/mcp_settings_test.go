package main

import (
	"strings"
	"testing"

	"github.com/OXeu/shinona/internal/settings"
)

func TestMCPSettingsBelongToTheAccount(t *testing.T) {
	t.Setenv("SHINONA_MCP_SERVERS", `[{"name":"startup","url":"https://example.test/mcp","token":"startup-secret"}]`)
	app, repo, query := settingsTestApp(t, nil)
	ctx := t.Context()
	const fields = "{ overridden servers { name url hasToken oauth } }"
	const read = "{ mcpSettings " + fields + " composerOptions { mcpServers } }"
	const save = "mutation ($input: McpServerInput!) { saveMcpServer(input: $input) " + fields + " }"
	if response := query("alice", read, nil); !strings.Contains(response, `"servers":[]`) || strings.Contains(response, "startup") {
		t.Fatalf("account initialized with shared MCP settings: %s", response)
	}
	input := map[string]any{"name": "shared-name", "url": "https://alice.example.test/mcp", "token": "alice-secret"}
	vars := map[string]any{"input": input}
	if response := query("alice", save, vars); !strings.Contains(response, `"hasToken":true`) || strings.Contains(response, "alice-secret") {
		t.Fatalf("personal MCP write did not mask its credential: %s", response)
	}
	if response := query("bob", read, nil); !strings.Contains(response, `"servers":[]`) || strings.Contains(response, "alice") {
		t.Fatalf("second account saw first account's MCP config: %s", response)
	}
	input["url"], input["token"] = "https://bob.example.test/mcp", "bob-secret"
	if response := query("bob", save, vars); !strings.Contains(response, "bob.example.test") || strings.Contains(response, "bob-secret") {
		t.Fatalf("second account could not reuse the name: %s", response)
	}
	if response := query("alice", read, nil); !strings.Contains(response, `"mcpServers":["shared-name"]`) || strings.Contains(response, "bob.example.test") {
		t.Fatalf("composer did not use the current account's MCP settings: %s", response)
	}
	delete(input, "token")
	input["previousName"], input["name"], input["url"] = "shared-name", "renamed", "https://alice.example.test/mcp"
	if response := query("alice", save, vars); !strings.Contains(response, `"name":"renamed"`) || !strings.Contains(response, `"hasToken":true`) {
		t.Fatalf("renaming lost the account's token: %s", response)
	}
	reloaded, err := newRuntime(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reloaded.accountMCP(ctx, "alice")
	if err != nil || len(stored) != 1 || stored[0].Name != "renamed" || stored[0].Token != "alice-secret" {
		t.Fatalf("account's MCP settings did not survive reload: %v", err)
	}
	if server, ok := app.lookupMCP(ctx, "bob", "renamed"); ok || server.URL != "" {
		t.Fatal("MCP OAuth could resolve another account's server")
	}
	input["previousName"], input["url"] = "renamed", "https://another.example.test/mcp"
	if response := query("alice", save, vars); !strings.Contains(response, "MCP_TOKEN_REQUIRED_FOR_URL_CHANGE") {
		t.Fatalf("saved token could be carried to another endpoint: %s", response)
	}
	input["token"], input["url"] = "", "http://127.0.0.1:8383/mcp"
	if response := query("alice", save, vars); !strings.Contains(response, "PRIVATE_ENDPOINT") {
		t.Fatalf("user MCP accepted a loopback endpoint: %s", response)
	}
	input["url"], input["oauth"], input["name"] = "https://alice.example.test/mcp", true, "shared-name"
	if response := query("alice", save, vars); !strings.Contains(response, `"oauth":true`) {
		t.Fatalf("cannot save OAuth without client credentials: %s", response)
	}
	input["url"], input["previousName"] = "https://bob.example.test/mcp", "shared-name"
	if response := query("bob", save, vars); !strings.Contains(response, `"oauth":true`) {
		t.Fatalf("cannot save OAuth for second account: %s", response)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := repo.SaveMCPGrant(ctx, owner, "shared-name", []byte("grant-fixture")); err != nil {
			t.Fatal(err)
		}
	}
	const reset = "mutation { resetMcpSettings " + fields + " }"
	if response := query("alice", reset, nil); !strings.Contains(response, `"servers":[]`) {
		t.Fatalf("clear personal MCP settings failed: %s", response)
	}
	aliceGrant, err := repo.MCPGrant(ctx, "alice", "shared-name")
	if err != nil || aliceGrant != nil {
		t.Fatalf("removing MCP settings retained the same account's grant: %v", err)
	}
	bobGrant, err := repo.MCPGrant(ctx, "bob", "shared-name")
	if err != nil || string(bobGrant) != "grant-fixture" {
		t.Fatalf("removing MCP settings deleted another account's grant: %v", err)
	}
	if response := query("bob", read, nil); !strings.Contains(response, "bob.example.test") {
		t.Fatalf("clearing alice's config changed bob's config: %s", response)
	}
	legacy, err := repo.SettingOverrides(ctx)
	if err != nil || legacy[settings.MCPServers] != "" {
		t.Fatalf("account MCP writes changed instance settings: %v", err)
	}
}
