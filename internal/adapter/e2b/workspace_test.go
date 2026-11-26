package e2b

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

// The generated stubs are private to the SDK. The fixture speaks Connect JSON.
type startRequest struct {
	Process struct {
		Cmd string
		Cwd string
	}
	PTY json.RawMessage
	Tag string
}

func readStart(t *testing.T, r *http.Request) startRequest {
	t.Helper()
	var request startRequest
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) < 5 || body[0] != 0 {
		t.Errorf("read envd Start request: %v", err)
		return request
	}
	size := int(binary.BigEndian.Uint32(body[1:5]))
	if size != len(body)-5 {
		t.Error("invalid envd Start frame length")
		return request
	}
	if err := json.Unmarshal(body[5:], &request); err != nil {
		t.Errorf("decode envd Start request: %v", err)
	}
	return request
}

func processFrame(t *testing.T, w http.ResponseWriter, flags byte, payload []byte) {
	t.Helper()
	header := binary.BigEndian.AppendUint32([]byte{flags}, uint32(len(payload)))
	if _, err := w.Write(append(header, payload...)); err != nil {
		t.Error(err)
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Error(err)
	}
}

func beginProcess(t *testing.T, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/connect+json")
	processFrame(t, w, 0, []byte(`{"event":{"start":{"pid":17}}}`))
}

func processOutput(t *testing.T, w http.ResponseWriter, kind, text string) {
	data := base64.StdEncoding.EncodeToString([]byte(text))
	processFrame(t, w, 0, fmt.Appendf(nil, `{"event":{"data":{"%s":"%s"}}}`, kind, data))
}

func finishProcess(t *testing.T, w http.ResponseWriter, exit int) {
	processFrame(t, w, 0, fmt.Appendf(nil, `{"event":{"end":{"exitCode":%d,"exited":true}}}`, exit))
	processFrame(t, w, 2, []byte("{}"))
}

func TestWorkspaceUsesSDKForFilesCommandsAndTerminal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "team-key" || r.Header.Get("E2b-Traffic-Access-Token") != "" {
			t.Error("control endpoint received incorrect credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"sandboxID":"sdk-test","envdVersion":"0.5.7","envdAccessToken":"data-key","trafficAccessToken":"route-key"}`)
	})
	taggedStops := make(chan bool, 4)
	ptyExit := make(chan struct{}, 1)
	data := http.NewServeMux()
	data.HandleFunc("/envd/files", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "/workspace/src/hello.txt" || r.URL.Query().Get("username") != "user" {
			t.Errorf("unexpected file parameters: %s", r.URL.RawQuery)
		}
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "hello" || r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Errorf("unexpected upload: %q, %v", body, err)
			}
		}
		_, _ = io.WriteString(w, "hello")
	})
	data.HandleFunc("POST /envd/process.Process/Start", func(w http.ResponseWriter, r *http.Request) {
		request := readStart(t, r)
		if request.Process.Cwd != "/workspace" || request.Tag == "" {
			t.Error("command lost working directory or process tag")
		}
		cmd := request.Process.Cmd
		beginProcess(t, w)
		switch {
		case len(request.PTY) != 0:
			processOutput(t, w, "pty", "terminal")
			select {
			case <-ptyExit:
				finishProcess(t, w, 0)
			case <-r.Context().Done():
			}
		case cmd == "slow":
			processOutput(t, w, "stdout", "progress")
			processOutput(t, w, "stderr", "still running")
			<-r.Context().Done()
		case cmd == "interrupted":
			processFrame(t, w, 2, []byte("{}"))
		default:
			processOutput(t, w, "stdout", strings.Repeat("a", sandbox.DefaultOutputLimit+1))
			finishProcess(t, w, 7)
		}
	})
	data.HandleFunc("POST /envd/process.Process/SendSignal", func(w http.ResponseWriter, r *http.Request) {
		var signal struct{ Process struct{ Tag string } }
		if err := json.NewDecoder(r.Body).Decode(&signal); err != nil {
			t.Errorf("decode envd SendSignal request: %v", err)
		}
		taggedStops <- signal.Process.Tag != ""
		envdOK(w)
	})
	data.HandleFunc("POST /envd/process.Process/Update", func(w http.ResponseWriter, _ *http.Request) {
		envdOK(w)
	})
	data.HandleFunc("POST /envd/process.Process/SendInput", func(w http.ResponseWriter, _ *http.Request) {
		ptyExit <- struct{}{}
		envdOK(w)
	})
	mux.Handle("/envd/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		if r.Header.Get("X-API-Key") != "" || r.Header.Get("X-Access-Token") != "data-key" ||
			r.Header.Get("E2b-Traffic-Access-Token") != "route-key" || r.Header.Get("E2b-Sandbox-Id") != "sdk-test" || user != "user" {
			t.Error("wrong credentials for envd request")
		}
		data.ServeHTTP(w, r)
	}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	store := openStore(t, ":memory:")
	seedWorkspaceOwner(t, store, "fixture", "")
	provider, err := New(Config{APIKey: "team-key", BaseURL: server.URL, ProxyURL: server.URL + "/envd",
		APIVersion: "v2", WorkspaceUser: "user", WorkDirectory: "/workspace"}, server.Client(), store.E2B)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	ws, err := provider.Acquire(ctx, sandbox.Spec{Policy: sandbox.Shared, OwnerID: "fixture", RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := ws.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	if err := ws.WriteFile(ctx, "src/hello.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if data, err := ws.ReadFile(ctx, "src/hello.txt"); err != nil || string(data) != "hello" {
		t.Fatalf("read: %q, %v", data, err)
	}
	if _, err := ws.ReadFile(ctx, "../above"); err == nil {
		t.Fatal("path escaped the workspace")
	}
	result, err := ws.Exec(ctx, sandbox.Command{Program: "false"})
	if err != nil || result.ExitCode != 7 || len(result.Stdout) != sandbox.DefaultOutputLimit || !result.Truncated {
		t.Fatalf("unexpected exit %d, bytes=%d, truncated=%v, err=%v", result.ExitCode, len(result.Stdout), result.Truncated, err)
	}
	if _, err := ws.Exec(ctx, sandbox.Command{Program: "interrupted"}); err == nil {
		t.Fatal("command stream without an exit was accepted")
	}
	requireTaggedStop(t, taggedStops)

	deadline, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	result, err = ws.Exec(deadline, sandbox.Command{Program: "slow"})
	if !errors.Is(err, context.DeadlineExceeded) || result.Stdout != "progress" || result.Stderr != "still running" {
		t.Fatalf("deadline lost partial output: %+v, %v", result, err)
	}
	requireTaggedStop(t, taggedStops)

	host := ws.(sandbox.TerminalWorkspace)
	terminal, err := host.OpenTerminal(ctx, sandbox.TerminalSize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len("terminal"))
	if _, err := io.ReadFull(terminal, buffer); err != nil || string(buffer) != "terminal" {
		t.Fatalf("terminal output: %q, %v", buffer, err)
	}
	if err := terminal.Resize(ctx, sandbox.TerminalSize{Cols: 100, Rows: 28}); err != nil {
		t.Fatal(err)
	}
	if err := terminal.Input(ctx, []byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Read(buffer); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected terminal exit: %v", err)
	}
	if err := terminal.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-taggedStops:
		t.Fatal("terminal that exited normally was killed again")
	default:
	}
}

func envdOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{}")
}

func requireTaggedStop(t *testing.T, stopped <-chan bool) {
	t.Helper()
	select {
	case tagged := <-stopped:
		if !tagged {
			t.Fatal("cleanup did not use the process tag")
		}
	default:
		t.Fatal("no cleanup request was sent")
	}
}
