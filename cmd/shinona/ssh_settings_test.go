package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
	"github.com/OXeu/shinona/internal/timing"
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
	wrongHostKey, wrongKey := sshTestKey(t)
	rawKey, err := ssh.ParseRawPrivateKey([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	encryptedKey, err := ssh.MarshalPrivateKeyWithPassphrase(rawKey, "", []byte("test passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ field, value, code string }{
		{"address", "missing-port.example.test", "SSH_INVALID_ADDRESS"},
		{"address", "127.0.0.2:22", "SSH_PRIVATE_ADDRESS"},
		{"user", " ", "SSH_INVALID_USER"},
		{"hostFingerprint", "SHA256:short", "SSH_INVALID_FINGERPRINT"},
		{"hostFingerprint", ssh.FingerprintSHA256(wrongHostKey.PublicKey()), "SSH_HOST_KEY_MISMATCH"},
		{"privateKey", "ssh-ed25519 public-key-only", "SSH_INVALID_PRIVATE_KEY"},
		{"privateKey", string(pem.EncodeToMemory(encryptedKey)), "SSH_ENCRYPTED_KEY"},
		{"privateKey", wrongKey, "SSH_AUTHENTICATION_FAILED"},
		{"user", "wrong-user", "SSH_AUTHENTICATION_FAILED"},
	} {
		draft := maps.Clone(input)
		draft[tc.field] = tc.value
		response := query("alice", testConnection, map[string]any{"input": draft})
		if !strings.Contains(response, tc.code) || strings.Contains(response, "PRIVATE KEY-----") {
			t.Fatalf("SSH test with invalid %s should report %s: %s", tc.field, tc.code, response)
		}
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
	if response := query("alice", testConnection, vars); !strings.Contains(response, "SSH_CONNECTION_REFUSED") {
		t.Fatalf("test a closed SSH port: %s", response)
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

func TestRetainedSSHWorkspaceReusesConnection(t *testing.T) {
	hostKey, key := sshTestKey(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("SHINONA_TRUSTED_SSH_ADDRESSES", listener.Addr().String())
	app, repo, _ := settingsTestApp(t, nil)
	server := settings.SSHServer{
		ID: "test-host", Name: "Test host", Address: listener.Addr().String(), User: "worker",
		Template: "alpine:3", HostFingerprint: ssh.FingerprintSHA256(hostKey.PublicKey()), PrivateKey: key,
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := repo.SaveSSHServer(t.Context(), owner, server, true); err != nil {
			t.Fatal(err)
		}
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, public ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(public.Marshal(), hostKey.PublicKey().Marshal()) {
			return nil, errors.New("unexpected SSH key")
		}
		return nil, nil
	}}
	config.AddHostKey(hostKey)
	var paused atomic.Bool
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- conn
			go serveSSHTestConnection(conn, config, func(command string) (string, uint32, bool) {
				if !strings.HasPrefix(command, "exec docker 'ps' ") {
					return "", 0, false
				}
				if paused.Load() {
					return "container-id paused\n", 0, true
				}
				return "container-id running\n", 0, true
			})
		}
	}()
	nextConnection := func() net.Conn {
		t.Helper()
		select {
		case conn := <-accepted:
			t.Cleanup(func() { _ = conn.Close() })
			return conn
		default:
			t.Fatal("expected a new SSH connection")
			return nil
		}
	}
	open := func(owner string) (sandbox.Workspace, []timing.Metric, error) {
		t.Helper()
		ctx, recorder := timing.WithRecorder(t.Context())
		target, err := app.ResolveForAccount(ctx, owner, settings.SSHProviderPrefix+server.ID)
		if err != nil {
			t.Fatal(err)
		}
		provider, ok := target.Provider.(sandbox.ProjectProvider)
		if !ok {
			t.Fatal("expected a project provider")
		}
		ws, err := provider.OpenProject(ctx, sandbox.Spec{OwnerID: owner})
		return ws, recorder.Metrics(), err
	}
	release := func(owner string, wantConnect bool) sandbox.Workspace {
		t.Helper()
		ws, metrics, err := open(owner)
		if err != nil {
			t.Fatal(err)
		}
		if err := ws.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		hasMetric := func(name string) bool {
			return slices.ContainsFunc(metrics, func(metric timing.Metric) bool { return metric.Name == name })
		}
		if !hasMetric("ssh_project_status") || hasMetric("ssh_connect") != wantConnect {
			t.Fatalf("unexpected SSH timings: %+v", metrics)
		}
		return ws
	}

	first := release("alice", true)
	conn := nextConnection()
	release("alice", false)
	if _, err := first.ReadFile(t.Context(), "file"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("released workspace could still open a channel: %v", err)
	}
	paused.Store(true)
	if _, _, err := open("alice"); !errors.Is(err, sandbox.ErrWorkspaceMissing) {
		t.Fatalf("did not recheck the container state: %v", err)
	}
	paused.Store(false)
	if len(accepted) != 0 {
		t.Fatal("opened another connection for the same account and credentials")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	release("alice", true)
	nextConnection()

	release("bob", true)
	nextConnection()
	server.User = "new-user"
	if err := repo.SaveSSHServer(t.Context(), "alice", server, false); err != nil {
		t.Fatal(err)
	}
	release("alice", true)
	nextConnection()
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
		serveSSHTestConnection(conn, config, func(command string) (string, uint32, bool) {
			ok := command == "exec docker 'info' '--format' '{{.ServerVersion}}'"
			if dockerAvailable.Load() {
				return "", 0, ok
			}
			return "", 1, ok
		})
	}
}

func serveSSHTestConnection(conn net.Conn, config *ssh.ServerConfig, execute func(string) (string, uint32, bool)) {
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
			var output string
			var status struct{ Code uint32 }
			var ok bool
			if request.Type == "exec" && ssh.Unmarshal(request.Payload, &payload) == nil {
				output, status.Code, ok = execute(payload.Command)
			}
			_ = request.Reply(ok, nil)
			if ok {
				_, _ = io.WriteString(channel, output)
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(status))
			}
		}
		_ = channel.Close()
	}
}
