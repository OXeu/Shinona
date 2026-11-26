package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/e2b"
	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/adapter/sshsandbox"
	"github.com/OXeu/shinona/internal/adapter/terminal"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

type benchmarkSandboxProvider struct {
	name string
	open func(*testing.B, *sqlite.Repository) sandbox.ManagedProjectProvider
}

func BenchmarkSandboxAPI(b *testing.B) {
	for _, backend := range []benchmarkSandboxProvider{
		{name: "ssh", open: benchmarkSSHProvider},
		{name: "e2b", open: benchmarkE2BProvider},
	} {
		b.Run(backend.name, func(b *testing.B) {
			for _, operation := range []struct {
				name string
				run  func(*testing.B, *benchmarkSandbox)
			}{
				{"GitChanges", benchmarkGitChanges},
				{"FileList", benchmarkFileList},
				{"GitHistory", benchmarkGitHistory},
				{"Initialize", benchmarkSandboxInitialize},
				{"Recreate", benchmarkSandboxRecreate},
				{"Destroy", benchmarkSandboxDestroy},
				{"PTYRoundTrip", benchmarkPTY},
			} {
				b.Run(operation.name, func(b *testing.B) {
					repo := benchmarkRepository(b)
					provider := backend.open(b, repo)
					operation.run(b, newBenchmarkSandbox(b, repo, backend.name, provider))
				})
			}
		})
	}
}

func benchmarkSSHProvider(b *testing.B, _ *sqlite.Repository) sandbox.ManagedProjectProvider {
	b.Helper()
	address := os.Getenv("SHINONA_BENCH_SSH_ADDRESS")
	if address == "" {
		b.Skip("set SHINONA_BENCH_SSH_ADDRESS to use a disposable SSH/Docker benchmark host")
	}
	provider, err := sshsandbox.New(sshsandbox.Config{
		Address:         address,
		User:            os.Getenv("SHINONA_BENCH_SSH_USER"),
		KeyPath:         os.Getenv("SHINONA_BENCH_SSH_KEY_PATH"),
		HostFingerprint: os.Getenv("SHINONA_BENCH_SSH_HOST_FINGERPRINT"),
		Template:        os.Getenv("SHINONA_BENCH_SSH_IMAGE"),
	})
	if err != nil {
		b.Fatal(err)
	}
	return provider
}

func benchmarkE2BProvider(b *testing.B, repo *sqlite.Repository) sandbox.ManagedProjectProvider {
	b.Helper()
	key := os.Getenv("SHINONA_BENCH_E2B_API_KEY")
	if key == "" {
		b.Skip("set SHINONA_BENCH_E2B_API_KEY to use a dedicated E2B benchmark account")
	}
	template := os.Getenv("SHINONA_BENCH_E2B_TEMPLATE")
	if template == "" {
		b.Fatal("set SHINONA_BENCH_E2B_TEMPLATE to a benchmark template with bash and git")
	}
	provider, err := e2b.New(e2b.Config{
		APIKey: key, Template: template, Timeout: 5 * time.Minute, OnTimeout: sandbox.TimeoutKill,
		BaseURL:  os.Getenv("SHINONA_BENCH_E2B_API_URL"),
		ProxyURL: os.Getenv("SHINONA_BENCH_E2B_PROXY_URL"),
		Domain:   os.Getenv("SHINONA_BENCH_E2B_DOMAIN"),
	}, nil, repo)
	if err != nil {
		b.Fatal(err)
	}
	return provider
}

type benchmarkSandbox struct {
	*benchmarkAPI
	provider sandbox.ManagedProjectProvider
	spec     sandbox.Spec
}

func newBenchmarkSandbox(b *testing.B, repo *sqlite.Repository, name string, provider sandbox.ManagedProjectProvider) *benchmarkSandbox {
	b.Helper()
	api := newBenchmarkAPI(b, repo, benchmarkTarget(name, provider, demo.Agent{}))
	project, err := api.service.CreateProject(b.Context(), api.owner, "Sandbox benchmark")
	if err != nil {
		b.Fatal(err)
	}
	if err := repo.SaveProjectSandboxConfig(b.Context(), api.owner, project.ID, run.ProjectSandboxConfig{Provider: name}); err != nil {
		b.Fatal(err)
	}
	spec := sandbox.Spec{OwnerID: api.owner, ProjectID: project.ID, Policy: sandbox.Shared}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(b.Context()), 30*time.Second)
		defer cancel()
		if err := api.service.CloseTerminals(ctx); err != nil {
			b.Error(err)
		}
		if err := provider.DestroyProject(ctx, spec); err != nil {
			b.Errorf("clean up benchmark sandbox for project %s: %v", spec.ProjectID, err)
		}
	})
	return &benchmarkSandbox{api, provider, spec}
}

func (s *benchmarkSandbox) lifecycle(b *testing.B, method, suffix string) func() {
	b.Helper()
	path := rest.Prefix + "/projects/" + url.PathEscape(s.spec.ProjectID) + "/sandbox" + suffix
	return func() {
		b.Helper()
		s.rest(b, method, path, nil, http.StatusNoContent, nil)
	}
}

func (s *benchmarkSandbox) requireState(b *testing.B, want sandbox.ProjectState) sandbox.ProjectStatus {
	b.Helper()
	ctx, cancel := context.WithTimeout(b.Context(), 15*time.Second)
	defer cancel()
	status, err := s.provider.ProjectStatus(ctx, s.spec)
	if err != nil || status.State != want {
		b.Fatalf("sandbox state = %+v, want %s: %v", status, want, err)
	}
	return status
}

func benchmarkSandboxInitialize(b *testing.B, s *benchmarkSandbox) {
	create, destroy := s.lifecycle(b, http.MethodPost, ""), s.lifecycle(b, http.MethodDelete, "")
	s.requireState(b, sandbox.ProjectNone)
	for b.Loop() {
		create()
		b.StopTimer()
		s.requireState(b, sandbox.ProjectRunning)
		destroy()
		s.requireState(b, sandbox.ProjectNone)
		b.StartTimer()
	}
}

func benchmarkSandboxRecreate(b *testing.B, s *benchmarkSandbox) {
	s.lifecycle(b, http.MethodPost, "")()
	previous := s.requireState(b, sandbox.ProjectRunning)
	recreate := s.lifecycle(b, http.MethodPost, "/recreate")
	for b.Loop() {
		recreate()
		b.StopTimer()
		current := s.requireState(b, sandbox.ProjectRunning)
		if current.ID == "" || current.ID == previous.ID {
			b.Fatalf("recreate preserved the previous sandbox ID: %q", current.ID)
		}
		previous = current
		b.StartTimer()
	}
}

func benchmarkSandboxDestroy(b *testing.B, s *benchmarkSandbox) {
	create, destroy := s.lifecycle(b, http.MethodPost, ""), s.lifecycle(b, http.MethodDelete, "")
	for b.Loop() {
		b.StopTimer()
		create()
		s.requireState(b, sandbox.ProjectRunning)
		b.StartTimer()

		destroy()
		b.StopTimer()
		s.requireState(b, sandbox.ProjectNone)
		b.StartTimer()
	}
}

func (s *benchmarkSandbox) gitFixture(b *testing.B) string {
	b.Helper()
	s.lifecycle(b, http.MethodPost, "")()
	session := s.session(b, s.spec.ProjectID)
	ctx, cancel := context.WithTimeout(b.Context(), 45*time.Second)
	defer cancel()
	ws, err := s.provider.OpenProject(ctx, s.spec)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := ws.Close(ctx); err != nil {
			b.Error(err)
		}
	}()
	result, err := ws.Exec(ctx, sandbox.Command{Program: "/bin/bash", Args: []string{"-ec", `
		mkdir benchmark-repo
		cd benchmark-repo
		git init -q -b main --template=
		git config user.name Benchmark
		git config user.email benchmark@example.invalid
		mkdir src
		for i in {1..40}; do printf 'fixture %s\n' "$i" > "src/file-$i.txt"; done
		git add src
		git commit -qm 'Fixture tree'
		for i in {1..11}; do
			printf 'version %s\n' "$i" > src/file-1.txt
			git commit -qam "Fixture version $i"
		done
		printf 'modified\n' >> src/file-1.txt
		printf 'untracked\n' > src/local.txt
	`}})
	if err != nil || result.ExitCode != 0 {
		b.Fatalf("seed sandbox repository: %+v, %v", result, err)
	}
	if err := s.repo.SetWorkspaceRoot(ctx, s.owner, session, s.spec.ProjectID, "benchmark-repo"); err != nil {
		b.Fatal(err)
	}
	return session
}

func benchmarkGitChanges(b *testing.B, s *benchmarkSandbox) {
	session := s.gitFixture(b)
	path := benchmarkSessionPath(session) + "/workspace/changes"
	var result struct {
		Branch    string
		Truncated bool
		Entries   []struct {
			Path, Status string
			Untracked    bool
		}
	}
	for b.Loop() {
		s.rest(b, http.MethodGet, path, nil, http.StatusOK, &result)
		if result.Branch != "main" || result.Truncated || len(result.Entries) != 2 ||
			result.Entries[0].Path != "src/file-1.txt" || result.Entries[1].Path != "src/local.txt" || !result.Entries[1].Untracked {
			b.Fatalf("unexpected git changes: %+v", result)
		}
	}
}

func benchmarkFileList(b *testing.B, s *benchmarkSandbox) {
	session := s.gitFixture(b)
	path := benchmarkSessionPath(session) + "/workspace/files?path=benchmark-repo%2Fsrc"
	var result struct {
		Truncated bool
		Entries   []struct {
			Path, Name string
			Directory  bool
		}
	}
	for b.Loop() {
		s.rest(b, http.MethodGet, path, nil, http.StatusOK, &result)
		if result.Truncated || len(result.Entries) != 41 {
			b.Fatalf("unexpected directory listing: %+v", result)
		}
	}
}

func benchmarkGitHistory(b *testing.B, s *benchmarkSandbox) {
	session := s.gitFixture(b)
	path := benchmarkSessionPath(session) + "/workspace/history"
	var result struct {
		Truncated bool
		Entries   []struct {
			Hash, Short, Author, Date, Subject string
		}
	}
	for b.Loop() {
		s.rest(b, http.MethodGet, path, nil, http.StatusOK, &result)
		if result.Truncated || len(result.Entries) != 12 || result.Entries[0].Subject != "Fixture version 11" {
			b.Fatalf("unexpected git history: %+v", result)
		}
	}
}

func benchmarkPTY(b *testing.B, s *benchmarkSandbox) {
	s.lifecycle(b, http.MethodPost, "")()
	session := s.session(b, s.spec.ProjectID)
	body, _ := s.post(b, terminal.Path+"/create", s.requestBody(b, map[string]string{"session": session}), http.StatusCreated)
	var tab run.TerminalTab
	if err := json.Unmarshal(body, &tab); err != nil || tab.ID == "" {
		b.Fatalf("create terminal: %s, %v", body, err)
	}
	query := url.Values{"session": {session}, "terminal": {tab.ID}}
	conn := s.websocket(b, terminal.Path+"/connect?"+query.Encode())
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		b.Fatal(err)
	}
	for {
		kind, payload, err := conn.ReadMessage()
		if err != nil {
			b.Fatal(err)
		}
		if kind != websocket.TextMessage {
			continue
		}
		var status struct{ State run.TerminalState }
		if err := json.Unmarshal(payload, &status); err != nil {
			b.Fatal(err)
		}
		if status.State == run.TerminalRunning {
			break
		}
		if status.State != run.TerminalStarting {
			b.Fatalf("terminal failed before opening: %s", status.State)
		}
	}
	// The two arguments become adjacent only when the shell prints them, so
	// input echo cannot be mistaken for the actual PTY response.
	const marker = "shinona-benchmark-ready"
	roundTrip := func(command string) {
		if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
			b.Fatal(err)
		}
		benchmarkSocketWrite(b, conn, []byte(command))
		var tail []byte
		for {
			kind, payload, err := conn.ReadMessage()
			if err != nil {
				b.Fatal(err)
			}
			if kind == websocket.TextMessage {
				b.Fatalf("terminal changed state during round trip: %s", payload)
			}
			tail = append(tail, payload...)
			if bytes.Contains(tail, []byte(marker)) {
				return
			}
			// The output marker may cross two WebSocket messages.
			tail = tail[max(0, len(tail)-len(marker)+1):]
		}
	}
	command := "printf '%s%s\\n' 'shinona-benchmark-' 'ready'\r"
	roundTrip("stty -echo; " + command)
	for b.Loop() {
		roundTrip(command)
	}
}
