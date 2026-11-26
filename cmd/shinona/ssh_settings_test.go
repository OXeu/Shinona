package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
	"github.com/OXeu/shinona/internal/timing"
)

type pausedSSHMetadataWrite struct {
	settings.SSHRepository
	started chan struct{}
	resume  chan struct{}
}

func (repo *pausedSSHMetadataWrite) SaveSSHServer(ctx context.Context, owner string, server settings.SSHServer, create bool) error {
	if server.Name == "Metadata edit" {
		close(repo.started)
		<-repo.resume
	}
	return repo.SSHRepository.SaveSSHServer(ctx, owner, server, create)
}

func TestSSHMetadataEditDoesNotOverwriteConcurrentKeyRotation(t *testing.T) {
	app, repo, _ := settingsTestApp(t, nil)
	ctx, cancel := context.WithTimeout(auth.WithUser(t.Context(), auth.User{ID: "alice"}), 3*time.Second)
	defer cancel()
	hostKey, oldKey := sshTestKey(t)
	_, newKey := sshTestKey(t)
	saved, err := app.SaveSSHServer(ctx, settings.SSHServer{
		SSHServerInfo: settings.SSHServerInfo{Name: "Original", Address: "test.example.com:22", User: "worker",
			HostFingerprint: ssh.FingerprintSHA256(hostKey.PublicKey())},
		PrivateKey: oldKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	paused := &pausedSSHMetadataWrite{SSHRepository: repo, started: make(chan struct{}), resume: make(chan struct{})}
	app.sshRuntime.repo = paused
	release := sync.OnceFunc(func() { close(paused.resume) })
	defer release()
	mutated := saved
	mutated.Name = "Metadata edit"
	metadataDone := make(chan error, 1)
	go func() {
		_, err := app.SaveSSHServer(ctx, settings.SSHServer{SSHServerInfo: mutated})
		metadataDone <- err
	}()
	select {
	case <-paused.started:
	case <-ctx.Done():
		t.Fatal("the metadata write did not reach persistence")
	}
	rotationDone := make(chan error, 1)
	go func() {
		_, err := app.SaveSSHServer(ctx, settings.SSHServer{SSHServerInfo: saved, PrivateKey: newKey})
		rotationDone <- err
	}()
	select {
	case err := <-rotationDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("the pending metadata write blocked key rotation")
	}
	release()
	select {
	case err := <-metadataDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("the metadata update did not finish")
	}
	server, err := repo.SSHServer(ctx, "alice", saved.ID)
	if err != nil || server.Name != mutated.Name || server.PrivateKey != strings.TrimSpace(newKey) {
		t.Fatalf("SSH metadata save lost an update or rewrote the key: %v", err)
	}
}

func TestAccountsCanBindAndSelectMultipleSSHServers(t *testing.T) {
	app, repo, query := settingsTestApp(t, nil)
	hostKey, key := sshTestKey(t)
	const serversPath = "/settings/ssh/servers"
	input := map[string]any{"id": "", "name": "East", "address": "east.example.test:22", "user": "worker",
		"template": "alpine:3", "hostFingerprint": ssh.FingerprintSHA256(hostKey.PublicKey()), "privateKey": key}
	add := func() string {
		t.Helper()
		response := query("alice", http.MethodPut, serversPath, input)
		var decoded struct {
			ID string
		}
		if err := json.Unmarshal([]byte(response), &decoded); err != nil || decoded.ID == "" ||
			strings.Contains(response, "PRIVATE KEY") {
			t.Fatalf("save SSH server or redact key: %s", response)
		}
		return decoded.ID
	}
	first := add()
	input["name"], input["address"] = "West", "west.example.test:2222"
	second := add()
	if first == second {
		t.Fatal("SSH servers reused an ID")
	}
	list := func(owner string) string {
		return query(owner, http.MethodGet, "/settings/ssh", nil) + query(owner, http.MethodGet, "/projects/default/sandbox", nil)
	}
	alice := list("alice")
	if !strings.Contains(alice, "East") || !strings.Contains(alice, "West") || strings.Contains(alice, "PRIVATE KEY") {
		t.Fatalf("account SSH servers: %s", alice)
	}
	if bob := list("bob"); strings.Contains(bob, "East") || strings.Contains(bob, "West") || strings.Contains(bob, "errors") {
		t.Fatalf("cross-account SSH servers: %s", bob)
	}
	defaultServer, err := repo.DefaultSSHServer(t.Context(), "alice")
	if err != nil || defaultServer.ID != first || defaultServer.PrivateKey != key {
		t.Fatal("default SSH server did not resolve with the expected credential")
	}
	if serialized, err := json.Marshal(defaultServer); err != nil || bytes.Contains(serialized, []byte("PRIVATE KEY")) {
		t.Fatal("SSH server could expose its credential when serialized")
	}
	provider := settings.SSHProviderPrefix + second
	if response := query("bob", http.MethodPut, "/projects/default/sandbox/config", sandboxInput(provider, "")); !strings.Contains(response, "SANDBOX_PROVIDER_UNAVAILABLE") {
		t.Fatalf("selected another account's SSH server: %s", response)
	}
	if response := query("alice", http.MethodPut, "/projects/default/sandbox/config", sandboxInput(provider, "")); response != "" {
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
	if response := query("alice", http.MethodDelete, serversPath+"/"+url.PathEscape(second), nil); !strings.Contains(response, "SSH_SERVER_IN_USE") {
		t.Fatalf("removed a configured project's SSH server: %s", response)
	}
	input["id"], input["name"], input["privateKey"] = second, "West renamed", ""
	newHostKey, _ := sshTestKey(t)
	input["address"], input["user"] = "new-west.example.test:22", "new-user"
	input["hostFingerprint"] = ssh.FingerprintSHA256(newHostKey.PublicKey())
	if response := query("alice", http.MethodPut, serversPath, input); !strings.Contains(response, "West renamed") {
		t.Fatalf("edit server without replacing its key: %s", response)
	}
	stored, err := repo.SSHServer(t.Context(), "alice", second)
	if err != nil || stored.PrivateKey != key || stored.Address != input["address"] ||
		stored.User != input["user"] || stored.HostFingerprint != input["hostFingerprint"] {
		t.Fatal("editing an in-use SSH server did not update its connection settings and preserve its key")
	}
	if response := query("alice", http.MethodDelete, serversPath+"/"+url.PathEscape(first), nil); response != "" {
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

	const testConnection = "/settings/ssh/connection-test"
	input := map[string]any{"id": "", "name": "", "address": listener.Addr().String(), "user": "worker",
		"template": "", "hostFingerprint": ssh.FingerprintSHA256(hostKey.PublicKey()), "privateKey": key}
	if response := query("alice", http.MethodPost, testConnection, input); !strings.Contains(response, "SSH_DOCKER_UNAVAILABLE") {
		t.Fatalf("check inaccessible Docker: %s", response)
	}
	dockerAvailable.Store(true)
	if response := query("alice", http.MethodPost, testConnection, input); response != "" {
		t.Fatalf("test draft with reachability, authentication and Docker: %s", response)
	}
	if servers, err := repo.SSHServers(t.Context(), "alice"); err != nil || len(servers) != 0 {
		t.Fatalf("test should not save the draft: %v", err)
	}

	input["name"] = "Test host"
	response := query("alice", http.MethodPut, "/settings/ssh/servers", input)
	var decoded struct {
		ID string
	}
	if err := json.Unmarshal([]byte(response), &decoded); err != nil || decoded.ID == "" {
		t.Fatalf("save SSH server: %s", response)
	}
	id := decoded.ID
	input["id"], input["privateKey"] = id, ""
	if response := query("alice", http.MethodPost, testConnection, input); response != "" {
		t.Fatalf("test SSH server with its saved key: %s", response)
	}
	if response := query("bob", http.MethodPost, testConnection, input); !strings.Contains(response, "SSH_SERVER_NOT_FOUND") {
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
		response := query("alice", http.MethodPost, testConnection, draft)
		if !strings.Contains(response, tc.code) || strings.Contains(response, "PRIVATE KEY-----") {
			t.Fatalf("SSH test with invalid %s should report %s: %s", tc.field, tc.code, response)
		}
	}

	const saveSandbox = "/projects/default/sandbox/config"
	provider := settings.SSHProviderPrefix + id
	if response := query("alice", http.MethodPut, saveSandbox, sandboxInput(provider, "")); response != "" {
		t.Fatalf("assign SSH provider: %s", response)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if response := query("alice", http.MethodPost, testConnection, input); !strings.Contains(response, "SSH_CONNECTION_REFUSED") {
		t.Fatalf("test a closed SSH port: %s", response)
	}
	if response := query("alice", http.MethodPut, saveSandbox, sandboxInput("e2b", "")); !strings.Contains(response, "PROJECT_SANDBOX_CLEANUP_FAILED") {
		t.Fatalf("switch requires confirmation when previous sandbox cannot be reached: %s", response)
	}
	if response := query("alice", http.MethodPut, saveSandbox, sandboxInput("e2b", "ssh")); !strings.Contains(response, "INVALID_PROJECT_SANDBOX") {
		t.Fatalf("skip cleanup for a different provider: %s", response)
	}
	if config, err := repo.ProjectSandboxConfig(t.Context(), "alice", ""); err != nil || config == nil || config.Provider != provider {
		t.Fatalf("project should still use the previous provider: %+v, %v", config, err)
	}
	if response := query("alice", http.MethodPut, saveSandbox, sandboxInput("e2b", provider)); response != "" {
		t.Fatalf("switch to E2B after confirming cleanup can be skipped: %s", response)
	}
	if config, err := repo.ProjectSandboxConfig(t.Context(), "alice", ""); err != nil || config == nil || config.Provider != "e2b" {
		t.Fatalf("project should now use E2B: %+v, %v", config, err)
	}
}

func sandboxInput(provider, skip string) map[string]any {
	return map[string]any{
		"input": map[string]string{"provider": provider}, "skipCleanupFor": skip,
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
	app, repo, query := settingsTestApp(t, nil)
	server := settings.SSHServer{
		SSHServerInfo: settings.SSHServerInfo{
			ID: "test-host", Name: "Test host", Address: listener.Addr().String(), User: "worker",
			Template: "alpine:3", HostFingerprint: ssh.FingerprintSHA256(hostKey.PublicKey()),
		},
		PrivateKey: key,
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
	var state atomic.Value
	state.Store("running")
	var statusCalls, listCalls atomic.Int32
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- conn
			go serveSSHTestConnection(conn, config, func(command string) (string, uint32, bool) {
				current := state.Load().(string)
				switch {
				case strings.HasPrefix(command, "exec docker 'ps' "):
					statusCalls.Add(1)
					if current == "" {
						return "", 0, true
					}
					return "container-id " + current + " hardened\n", 0, true
				case strings.HasPrefix(command, "exec docker 'exec' ") && strings.Contains(command, " 'shinona-list' "):
					listCalls.Add(1)
					if current != "running" || strings.HasSuffix(command, " 'missing'") {
						return "", 2, true
					}
					return "d\x00src\x00f\x00README.md\x00", 0, true
				default:
					return "", 0, false
				}
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
	target, err := app.ResolveForAccount(t.Context(), "alice", settings.SSHProviderPrefix+server.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := app.ResolveForAccount(t.Context(), "alice", settings.SSHProviderPrefix+server.ID)
	if err != nil || target.Provider != resolved.Provider {
		t.Fatalf("unchanged SSH settings rebuilt the provider: %v", err)
	}
	for range 2 {
		ctx, recorder := timing.WithRecorder(t.Context())
		lease, err := target.Provider.Acquire(ctx, sandbox.Spec{OwnerID: "alice", Policy: sandbox.Shared})
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(recorder.Metrics(), func(metric timing.Metric) bool { return metric.Name == "ssh_connect" }) {
			t.Fatal("agent lease did not reuse the SSH transport")
		}
	}
	if _, err := first.ReadFile(t.Context(), "file"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("released workspace could still open a channel: %v", err)
	}
	state.Store("paused")
	if _, _, err := open("alice"); !errors.Is(err, sandbox.ErrWorkspaceMissing) {
		t.Fatalf("did not recheck the container state: %v", err)
	}
	state.Store("running")
	if len(accepted) != 0 {
		t.Fatal("opened another connection for the same account and credentials")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	release("alice", true)
	conn = nextConnection()

	if err := repo.SaveProjectSandboxConfig(t.Context(), "alice", "", run.ProjectSandboxConfig{Provider: settings.SSHProviderPrefix + server.ID}); err != nil {
		t.Fatal(err)
	}
	var session struct{ ID string }
	created := query("alice", http.MethodPost, "/sessions", map[string]any{})
	if err := json.Unmarshal([]byte(created), &session); err != nil || session.ID == "" {
		t.Fatalf("create SSH session: %s", created)
	}
	list := func(folder, want string, wantChecks int32) {
		t.Helper()
		beforeLists, beforeChecks := listCalls.Load(), statusCalls.Load()
		response := query("alice", http.MethodGet, "/sessions/"+session.ID+"/workspace/files?path="+folder, nil)
		if !strings.Contains(response, want) {
			t.Fatalf("list %q: %s", folder, response)
		}
		if lists, checks := listCalls.Load()-beforeLists, statusCalls.Load()-beforeChecks; lists != 1 || checks != wantChecks {
			t.Fatalf("list %q made %d directory calls and %d status checks, want 1 and %d", folder, lists, checks, wantChecks)
		}
	}
	list(".", `"name":"README.md"`, 0)
	list("missing", "WORKSPACE_INVALID_PATH", 1)
	for _, unavailable := range []string{"paused", "exited", ""} {
		state.Store(unavailable)
		list(".", "WORKSPACE_UNAVAILABLE", 1)
	}
	state.Store("running")
	if len(accepted) != 0 {
		t.Fatal("directory requests did not reuse the connection")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	list(".", `"name":"README.md"`, 0)
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
