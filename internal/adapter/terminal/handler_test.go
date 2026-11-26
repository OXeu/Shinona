package terminal_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/adapter/terminal"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

func TestTerminalSocketReplaysBeforeRunningAndJoinsInputOnShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	const owner, session = "owner", "session"
	db, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	user := auth.User{ID: owner, Email: "owner@example.test"}
	if err := db.Accounts.CreatePasswordUser(ctx, user, []byte("fixture")); err != nil {
		t.Fatal(err)
	}
	if err := db.Conversations.CreateSession(ctx, run.Session{ID: session, OwnerID: owner}); err != nil {
		t.Fatal(err)
	}

	allowInputReturn := make(chan struct{})
	releaseInput := sync.OnceFunc(func() { close(allowInputReturn) })
	defer releaseInput()
	shell := &socketFixtureShell{
		initialOutput: strings.NewReader("prompt> "),
		inputStarted:  make(chan struct{}),
		inputCanceled: make(chan struct{}),
		releaseInput:  allowInputReturn,
	}
	service := run.NewService(db.RunStores(), func(context.Context, string, string) (run.Target, error) {
		return run.Target{Provider: socketFixtureProvider{shell: shell}}, nil
	}, func() string { return "tab" }, &run.Controls{}, nil, run.DefaultWorkspaceTimeouts())
	lifetime, shutdown := context.WithCancel(ctx)
	defer shutdown()
	tab, err := service.StartTerminal(ctx, lifetime, owner, session)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := service.Terminal(ctx, owner, session, tab.ID)
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, n, _, changed := entry.Output(0, make([]byte, len("prompt> ")), nil)
		if n == len("prompt> ") {
			break
		}
		await(t, ctx, changed)
	}

	const origin = "https://shinona.example.test"
	mux := http.NewServeMux()
	authenticated := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
		})
	}
	terminal.Routes(mux, lifetime, origin, service, authenticated, func(h http.Handler) http.Handler { return h },
		func(*http.Request) (*auth.User, error) { return &user, nil }, slog.New(slog.DiscardHandler))
	served := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(served)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + terminal.Path + "/connect?session=" + session + "&terminal=" + tab.ID
	socket, response, err := websocket.DefaultDialer.DialContext(ctx, url, http.Header{"Origin": {origin}})
	if response != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = socket.Close() }()
	deadline, _ := ctx.Deadline()
	if err := socket.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	kind, data, err := socket.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(data) != "prompt> " {
		t.Fatalf("first message must replay the prompt: %d %q %v", kind, data, err)
	}
	kind, data, err = socket.ReadMessage()
	var state struct{ State run.TerminalState }
	if err != nil || kind != websocket.TextMessage || json.Unmarshal(data, &state) != nil || state.State != run.TerminalRunning {
		t.Fatalf("running must follow replay: %d %q %v", kind, data, err)
	}
	if err := socket.WriteMessage(websocket.BinaryMessage, []byte("input")); err != nil {
		t.Fatal(err)
	}
	await(t, ctx, shell.inputStarted)
	shutdown()
	await(t, ctx, shell.inputCanceled)
	select {
	case <-served:
		t.Fatal("terminal handler returned while its input worker was still running")
	case <-time.After(20 * time.Millisecond):
	}
	releaseInput()
	await(t, ctx, served)
	if err := service.CloseTerminals(ctx); err != nil {
		t.Fatal(err)
	}
}

func await(t *testing.T, ctx context.Context, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

type socketFixtureProvider struct {
	sandbox.ProjectProvider
	shell *socketFixtureShell
}

func (p socketFixtureProvider) Acquire(context.Context, sandbox.Spec) (sandbox.Workspace, error) {
	return socketFixtureWorkspace{shell: p.shell}, nil
}

type socketFixtureWorkspace struct {
	sandbox.Workspace
	shell *socketFixtureShell
}

func (w socketFixtureWorkspace) OpenTerminal(ctx context.Context, _ sandbox.TerminalSize) (sandbox.Terminal, error) {
	w.shell.lifetime = ctx
	return w.shell, nil
}

func (socketFixtureWorkspace) Close(context.Context) error { return nil }

type socketFixtureShell struct {
	sandbox.Terminal
	lifetime      context.Context
	initialOutput *strings.Reader
	inputStarted  chan struct{}
	inputCanceled chan struct{}
	releaseInput  <-chan struct{}
}

func (s *socketFixtureShell) Read(buf []byte) (int, error) {
	if s.initialOutput.Len() != 0 {
		return s.initialOutput.Read(buf)
	}
	<-s.lifetime.Done()
	return 0, s.lifetime.Err()
}

func (s *socketFixtureShell) Input(ctx context.Context, _ []byte) error {
	close(s.inputStarted)
	<-ctx.Done()
	close(s.inputCanceled)
	<-s.releaseInput
	return ctx.Err()
}

func (*socketFixtureShell) Close(context.Context) error { return nil }
