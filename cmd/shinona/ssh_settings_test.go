package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
)

func TestAccountsCanBindAndSelectMultipleSSHServers(t *testing.T) {
	app, repo, query := settingsTestApp(t, nil)
	hostKey, key := sshTestKey(t)
	const save = `mutation ($input: SSHServerInput!) {
		saveSshServer(input: $input) { id name address user template hostFingerprint }
	}`
	input := map[string]any{"id": "", "name": "East", "address": "east.example.test:22", "user": "worker",
		"template": "alpine:3", "hostFingerprint": ssh.FingerprintSHA256(hostKey.PublicKey()), "privateKey": key}
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
	newHostKey, _ := sshTestKey(t)
	input["address"], input["user"] = "new-west.example.test:22", "new-user"
	input["hostFingerprint"] = ssh.FingerprintSHA256(newHostKey.PublicKey())
	if response := query("alice", save, map[string]any{"input": input}); !strings.Contains(response, "West renamed") {
		t.Fatalf("edit server without replacing its key: %s", response)
	}
	stored, err := repo.SSHServer(t.Context(), "alice", second)
	if err != nil || stored.PrivateKey != key || stored.Address != input["address"] ||
		stored.User != input["user"] || stored.HostFingerprint != input["hostFingerprint"] {
		t.Fatal("editing an in-use SSH server did not update its connection settings and preserve its key")
	}
	if response := query("alice", remove, map[string]any{"id": first}); !strings.Contains(response, `"removeSshServer":true`) {
		t.Fatalf("remove unused SSH server: %s", response)
	}
}

func TestSSHConnectionAndSwitchAfterCleanupFailure(t *testing.T) {
	hostKey, key := sshTestKey(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var dockerAvailable atomic.Bool
	go serveSSHTestHost(listener, hostKey, &dockerAvailable)
	t.Setenv("SHINONA_TRUSTED_SSH_ADDRESSES", listener.Addr().String())
	t.Setenv("E2B_API_KEY", "test-e2b-key")
	_, repo, query := settingsTestApp(t, nil)

	const testConnection = `mutation ($input: SSHServerInput!) { testSshServer(input: $input) }`
	input := map[string]any{"id": "", "name": "", "address": listener.Addr().String(), "user": "worker",
		"template": "", "hostFingerprint": ssh.FingerprintSHA256(hostKey.PublicKey()), "privateKey": key}
	vars := map[string]any{"input": input}
	if response := query("alice", testConnection, vars); !strings.Contains(response, "SSH_DOCKER_UNAVAILABLE") {
		t.Fatalf("check inaccessible Docker: %s", response)
	}
	dockerAvailable.Store(true)
	if response := query("alice", testConnection, vars); !strings.Contains(response, `"testSshServer":true`) {
		t.Fatalf("test draft with reachability, authentication and Docker: %s", response)
	}
	if servers, err := repo.SSHServers(t.Context(), "alice"); err != nil || len(servers) != 0 {
		t.Fatalf("test should not save the draft: %v", err)
	}

	input["name"] = "Test host"
	response := query("alice", `mutation ($input: SSHServerInput!) { saveSshServer(input: $input) { id } }`, vars)
	var decoded struct {
		Data struct{ SaveSshServer struct{ ID string } }
	}
	if err := json.Unmarshal([]byte(response), &decoded); err != nil || decoded.Data.SaveSshServer.ID == "" {
		t.Fatalf("save SSH server: %s", response)
	}
	id := decoded.Data.SaveSshServer.ID
	input["id"], input["privateKey"] = id, ""
	if response := query("alice", testConnection, vars); !strings.Contains(response, `"testSshServer":true`) {
		t.Fatalf("test SSH server with its saved key: %s", response)
	}
	if response := query("bob", testConnection, vars); !strings.Contains(response, "SSH_SERVER_NOT_FOUND") {
		t.Fatalf("test another account's SSH server: %s", response)
	}
	wrongHostKey, _ := sshTestKey(t)
	input["hostFingerprint"] = ssh.FingerprintSHA256(wrongHostKey.PublicKey())
	if response := query("alice", testConnection, vars); !strings.Contains(response, "SSH_CONNECTION_FAILED") {
		t.Fatalf("test a mismatched host key: %s", response)
	}

	const saveSandbox = `mutation ($provider: String!, $skip: String) {
		saveProjectSandbox(projectId:"", input:{provider:$provider, template:"", timeoutSeconds:0, onTimeout:""}, skipCleanupFor:$skip)
	}`
	provider := settings.SSHProviderPrefix + id
	if response := query("alice", saveSandbox, map[string]any{"provider": provider}); !strings.Contains(response, `"saveProjectSandbox":true`) {
		t.Fatalf("assign SSH provider: %s", response)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if response := query("alice", saveSandbox, map[string]any{"provider": "e2b"}); !strings.Contains(response, "PROJECT_SANDBOX_CLEANUP_FAILED") {
		t.Fatalf("switch requires confirmation when previous sandbox cannot be reached: %s", response)
	}
	if response := query("alice", saveSandbox, map[string]any{"provider": "e2b", "skip": "ssh"}); !strings.Contains(response, "INVALID_PROJECT_SANDBOX") {
		t.Fatalf("skip cleanup for a different provider: %s", response)
	}
	if config, err := repo.ProjectSandboxConfig(t.Context(), "alice", ""); err != nil || config == nil || config.Provider != provider {
		t.Fatalf("project should still use the previous provider: %+v, %v", config, err)
	}
	if response := query("alice", saveSandbox, map[string]any{"provider": "e2b", "skip": provider}); !strings.Contains(response, `"saveProjectSandbox":true`) {
		t.Fatalf("switch to E2B after confirming cleanup can be skipped: %s", response)
	}
	if config, err := repo.ProjectSandboxConfig(t.Context(), "alice", ""); err != nil || config == nil || config.Provider != "e2b" {
		t.Fatalf("project should now use E2B: %+v, %v", config, err)
	}
}

func sshTestKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer, strings.TrimSpace(string(pem.EncodeToMemory(block)))
}

func serveSSHTestHost(listener net.Listener, key ssh.Signer, dockerAvailable *atomic.Bool) {
	config := &ssh.ServerConfig{PublicKeyCallback: func(conn ssh.ConnMetadata, public ssh.PublicKey) (*ssh.Permissions, error) {
		if conn.User() != "worker" || !bytes.Equal(public.Marshal(), key.PublicKey().Marshal()) {
			return nil, errors.New("unexpected SSH user or key")
		}
		return nil, nil
	}}
	config.AddHostKey(key)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		serveSSHTestConnection(conn, config, dockerAvailable.Load())
	}
}

func serveSSHTestConnection(conn net.Conn, config *ssh.ServerConfig, dockerAvailable bool) {
	defer func() { _ = conn.Close() }()
	_, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		channel, requests, err := incoming.Accept()
		if err != nil {
			return
		}
		request, open := <-requests
		if open {
			var payload struct{ Command string }
			ok := request.Type == "exec" && ssh.Unmarshal(request.Payload, &payload) == nil &&
				payload.Command == "exec docker 'info' '--format' '{{.ServerVersion}}'"
			_ = request.Reply(ok, nil)
			if ok {
				var status struct{ Code uint32 }
				if !dockerAvailable {
					status.Code = 1
				}
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(status))
			}
		}
		_ = channel.Close()
	}
}
