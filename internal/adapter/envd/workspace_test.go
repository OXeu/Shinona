package envd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/OXeu/shinona/internal/sandbox"
)

func TestWorkspaceProtocol(t *testing.T) {
	type payload = map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "/workspace/src/hello.txt" || r.URL.Query().Get("username") != "user" {
			t.Errorf("unexpected file parameters: %s", r.URL.RawQuery)
		}
		if r.Method == http.MethodPost {
			if r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Error("upload should use binary content type")
			}
			data, err := io.ReadAll(r.Body)
			if err != nil || string(data) != "hello" {
				t.Errorf("unexpected upload: %q, %v", data, err)
			}
		}
		_, _ = io.WriteString(w, "hello")
	})
	mux.Handle("/process.Process/Start", connect.NewServerStreamHandler("/process.Process/Start",
		func(ctx context.Context, req *connect.Request[payload], stream *connect.ServerStream[payload]) error {
			process, ok := (*req.Msg)["process"].(map[string]any)
			if !ok || process["cwd"] != "/workspace" || (*req.Msg)["stdin"] != false || (*req.Msg)["tag"] == "" {
				t.Errorf("unexpected process config: %v", req.Msg)
			}
			if process["cmd"] == "slow" {
				data := payload{"event": payload{"data": payload{"stdout": []byte("progress"), "stderr": []byte("host timed out")}}}
				if err := stream.Send(&data); err != nil {
					return err
				}
				<-ctx.Done()
				return ctx.Err()
			}
			data := payload{"event": payload{"data": payload{"stdout": []byte(strings.Repeat("a", maxOutputBytes+1))}}}
			if err := stream.Send(&data); err != nil {
				return err
			}
			if process["cmd"] == "interrupted" {
				return nil
			}
			end := payload{"event": payload{"end": payload{"exitCode": 7, "exited": true}}}
			return stream.Send(&end)
		}, connect.WithCodec(jsonCodec{})))
	stopped := make(chan bool, 1)
	mux.Handle("/process.Process/SendSignal", connect.NewUnaryHandler("/process.Process/SendSignal",
		func(_ context.Context, req *connect.Request[payload]) (*connect.Response[payload], error) {
			process, _ := (*req.Msg)["process"].(map[string]any)
			stopped <- (*req.Msg)["signal"] == float64(9) && process["tag"] != ""
			return connect.NewResponse(&payload{}), nil
		}, connect.WithCodec(jsonCodec{})))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, ok := r.BasicAuth()
		if !ok || user != "user" || r.Header.Get("X-Access-Token") != "test-token" {
			t.Error("missing envd credentials")
			http.Error(w, "missing credentials", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer server.Close()

	workspace, err := New(server.Client(), Config{URL: server.URL, Token: "test-token", User: "user", Directory: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := workspace.WriteFile(ctx, "src/hello.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if data, err := workspace.ReadFile(ctx, "src/hello.txt"); err != nil || string(data) != "hello" {
		t.Fatalf("read: %q, %v", data, err)
	}
	if _, err := workspace.ReadFile(ctx, "../above"); err == nil {
		t.Fatal("expected an error for a path above the work directory")
	}
	result, err := workspace.Exec(ctx, sandbox.Command{Program: "/bin/false"})
	if err != nil || result.ExitCode != 7 || len(result.Stdout) != maxOutputBytes || !result.Truncated {
		t.Fatalf("unexpected command result: exit=%d, bytes=%d, truncated=%v, err=%v", result.ExitCode, len(result.Stdout), result.Truncated, err)
	}
	_, err = workspace.Exec(ctx, sandbox.Command{Program: "interrupted"})
	if err == nil || !strings.Contains(err.Error(), "before process exit") {
		t.Fatalf("expected stream failure, got %v", err)
	}
	select {
	case tagged := <-stopped:
		if !tagged {
			t.Fatal("expected SIGKILL for the command tag")
		}
	default:
		t.Fatalf("interrupted process received no cleanup request: %v", err)
	}

	deadline, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	result, err = workspace.Exec(deadline, sandbox.Command{Program: "slow"})
	if err == nil || !strings.Contains(err.Error(), "deadline") ||
		result.Stdout != "progress" || result.Stderr != "host timed out" {
		t.Fatalf("deadline must preserve output: %+v, %v", result, err)
	}
	select {
	case tagged := <-stopped:
		if !tagged {
			t.Fatal("expected SIGKILL for the timed-out command tag")
		}
	default:
		t.Fatal("timed-out process received no cleanup request")
	}
}
