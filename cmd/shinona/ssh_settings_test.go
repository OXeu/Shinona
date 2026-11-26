package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
)

func TestAccountsCanBindAndSelectMultipleSSHServers(t *testing.T) {
	app, repo, query := settingsTestApp(t, nil)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(string(pem.EncodeToMemory(block)))
	const save = `mutation ($input: SSHServerInput!) {
		saveSshServer(input: $input) { id name address user template hostFingerprint }
	}`
	input := map[string]any{"id": "", "name": "East", "address": "east.example.test:22", "user": "worker",
		"template": "alpine:3", "hostFingerprint": ssh.FingerprintSHA256(hostKey), "privateKey": key}
	add := func() string {
		t.Helper()
		response := query("alice", save, map[string]any{"input": input})
		var decoded struct {
			Data struct{ SaveSshServer struct{ ID string } }
		}
		if err := json.Unmarshal([]byte(response), &decoded); err != nil || decoded.Data.SaveSshServer.ID == "" ||
			strings.Contains(response, "PRIVATE KEY") {
			t.Fatalf("save SSH server or redact key: %s", response)
		}
		return decoded.Data.SaveSshServer.ID
	}
	first := add()
	input["name"], input["address"] = "West", "west.example.test:2222"
	second := add()
	if first == second {
		t.Fatal("SSH servers reused an ID")
	}
	const list = `{ sshServers { id name } projectSandbox(projectId: "") { providers { provider label } } }`
	alice := query("alice", list, nil)
	if !strings.Contains(alice, "East") || !strings.Contains(alice, "West") || strings.Contains(alice, "PRIVATE KEY") {
		t.Fatalf("account SSH servers: %s", alice)
	}
	if bob := query("bob", list, nil); strings.Contains(bob, "East") || strings.Contains(bob, "West") || strings.Contains(bob, "errors") {
		t.Fatalf("cross-account SSH servers: %s", bob)
	}
	const choose = `mutation ($provider: String!) {
		saveProjectSandbox(projectId:"", input:{provider:$provider, template:"", timeoutSeconds:0, onTimeout:""})
	}`
	provider := settings.SSHProviderPrefix + second
	if response := query("bob", choose, map[string]any{"provider": provider}); !strings.Contains(response, "SANDBOX_PROVIDER_UNAVAILABLE") {
		t.Fatalf("selected another account's SSH server: %s", response)
	}
	if response := query("alice", choose, map[string]any{"provider": provider}); !strings.Contains(response, `"saveProjectSandbox":true`) {
		t.Fatalf("select SSH server: %s", response)
	}
	config, err := repo.ProjectSandboxConfig(t.Context(), "alice", "")
	if err != nil || config == nil || *config != (run.ProjectSandboxConfig{Provider: provider}) {
		t.Fatalf("project SSH setting: %+v, %v", config, err)
	}
	target, err := app.ResolveForAccount(t.Context(), "alice", provider)
	if err != nil || target.Provider == nil || target.Label != "West" {
		t.Fatalf("resolve selected account provider: %+v, %v", target, err)
	}
	const remove = `mutation ($id: ID!) { removeSshServer(id:$id) }`
	if response := query("alice", remove, map[string]any{"id": second}); !strings.Contains(response, "SSH_SERVER_IN_USE") {
		t.Fatalf("removed a configured project's SSH server: %s", response)
	}
	input["id"], input["name"], input["privateKey"] = second, "West renamed", ""
	if response := query("alice", save, map[string]any{"input": input}); !strings.Contains(response, "West renamed") {
		t.Fatalf("edit server without replacing its key: %s", response)
	}
	stored, err := repo.SSHServer(t.Context(), "alice", second)
	if err != nil || stored.PrivateKey != key {
		t.Fatal("editing SSH server lost the existing private key")
	}
	if response := query("alice", remove, map[string]any{"id": first}); !strings.Contains(response, `"removeSshServer":true`) {
		t.Fatalf("remove unused SSH server: %s", response)
	}
}
