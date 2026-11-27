package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
)

func TestOpenRejectsDuplicateSelectionBeforeConnecting(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("invalid selection connected to an MCP server")
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(fixture.Close)
	servers := NewServers(settings.MCPConfig{{Name: "fixture", URL: fixture.URL}}, fixture.Client(), nil)
	tools, release, err := servers.Open(t.Context(), "owner", []string{"fixture", "fixture"})
	if !errors.Is(err, run.ErrOptions) || release != nil || len(tools) != 0 {
		t.Fatalf("open with duplicate selection returned %d tools, cleanup=%t, error=%v", len(tools), release != nil, err)
	}
}

func TestConfiguredStreamableHTTPTool(t *testing.T) {
	mcpServer := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	type Input struct {
		Text string `json:"text"`
	}
	sdk.AddTool(mcpServer, &sdk.Tool{Name: "echo"}, func(_ context.Context, _ *sdk.CallToolRequest, input Input) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: input.Text}}}, nil, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return mcpServer }, nil)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("MCP request is missing its credential")
		}
		handler.ServeHTTP(w, r)
	}))
	defer fixture.Close()
	servers := NewServers(settings.MCPConfig{{Name: "fixture", URL: fixture.URL, Token: "fixture"}}, fixture.Client(), nil)
	tools, closeTools, err := servers.Open(t.Context(), "owner", []string{"fixture"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeTools() }()
	if len(tools) != 1 || tools[0].Name != qualifiedToolName("fixture", "echo") {
		t.Fatalf("unexpected tools: %v", tools)
	}
	result, err := tools[0].Execute(t.Context(), map[string]any{"text": "hello"})
	if err != nil || result.Text != "hello" {
		t.Fatalf("call tool: %+v, %v", result, err)
	}
}

func TestMCPToolReleasesStreamWhenItsResultArrives(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "echo"}, func(_ context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "complete"}}}, nil, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	streamClosed := make(chan struct{}, 2)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			handler.ServeHTTP(w, r)
			return
		}
		data, err := io.ReadAll(r.Body)
		var request struct{ Method string }
		if err != nil || json.Unmarshal(data, &request) != nil {
			t.Error("could not read the MCP request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		handler.ServeHTTP(w, r)
		if request.Method == "tools/call" {
			if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
				t.Error("the fixture did not return an event stream")
			}
			// Keep the response open after the normal terminal result has
			// been sent. The client should close this stream on its own.
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			streamClosed <- struct{}{}
		}
	}))
	t.Cleanup(fixture.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	servers := NewServers(settings.MCPConfig{{Name: "fixture", URL: fixture.URL}}, fixture.Client(), nil)
	tools, release, err := servers.Open(ctx, "owner", []string{"fixture"})
	if err != nil || len(tools) != 1 {
		t.Fatalf("open fixture tool: %d tools, error=%v", len(tools), err)
	}
	defer func() { _ = release() }()
	for range 2 {
		result, err := tools[0].Execute(ctx, map[string]any{})
		if err != nil || result.Text != "complete" {
			t.Fatalf("call tool: %+v, %v", result, err)
		}
		select {
		case <-streamClosed:
			if ctx.Err() != nil {
				t.Fatal("the response stayed open until the parent context expired")
			}
		case <-ctx.Done():
			t.Fatal("the response stayed open after its result arrived")
		}
	}
}

func TestMCPToolDiscoveryReleasesOnePageBeforeTheNext(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, &sdk.ServerOptions{PageSize: 1})
	for _, name := range []string{"first", "second"} {
		sdk.AddTool(server, &sdk.Tool{Name: name}, func(_ context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: name}}}, nil, nil
		})
	}
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			handler.ServeHTTP(w, r)
			return
		}
		data, err := io.ReadAll(r.Body)
		var request struct{ Method string }
		if err != nil || json.Unmarshal(data, &request) != nil {
			t.Error("could not read the MCP request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		handler.ServeHTTP(w, r)
		if request.Method == "tools/list" {
			// The next page needs the same connection slot. Leave this SSE
			// response open after sending its complete page.
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	t.Cleanup(fixture.Close)
	client := *fixture.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 1
	client.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	servers := NewServers(settings.MCPConfig{{Name: "fixture", URL: fixture.URL}}, &client, nil)
	tools, closeTools, err := servers.Open(ctx, "owner", []string{"fixture"})
	if err != nil || len(tools) != 2 {
		t.Fatalf("opening a catalog with two pages: %d tools, %v", len(tools), err)
	}
	defer func() { _ = closeTools() }()
	for i, name := range []string{"first", "second"} {
		output, err := tools[i].Execute(ctx, nil)
		if err != nil || output.Text != name {
			t.Fatalf("tool %d after paginated discovery: %+v, %v", i, output, err)
		}
	}
}

func TestMCPToolDiscoveryStopsInvalidPagination(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		cursor     func(int) string
		pages      int
	}{
		{"repeated cursor", "repeated pagination cursor", func(int) string { return "second-page" }, 2},
		{"page budget", "page limit", strconv.Itoa, maxToolCatalogPages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
			var pages atomic.Int32
			server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
				return func(ctx context.Context, method string, request sdk.Request) (sdk.Result, error) {
					if method != "tools/list" {
						return next(ctx, method, request)
					}
					n := int(pages.Add(1))
					if n > tc.pages {
						return nil, errors.New("unexpected page request after the fixture ended")
					}
					return &sdk.ListToolsResult{NextCursor: tc.cursor(n), Tools: []*sdk.Tool{}}, nil
				}
			})
			fixture := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
				return server
			}, &sdk.StreamableHTTPOptions{JSONResponse: true}))
			t.Cleanup(fixture.Close)
			servers := NewServers(settings.MCPConfig{{Name: "fixture", URL: fixture.URL}}, fixture.Client(), nil)
			tools, closeTools, err := servers.Open(t.Context(), "owner", []string{"fixture"})
			if closeTools != nil {
				defer func() { _ = closeTools() }()
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(tools) != 0 || int(pages.Load()) != tc.pages {
				t.Fatalf("catalog discovery: %d pages, %d tools, %v", pages.Load(), len(tools), err)
			}
		})
	}
}

func TestMCPIncompleteJSONResponseKeepsSessionUsable(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		testMCPIncompleteJSONResponse(t, 0)
	})
	t.Run("deadline", func(t *testing.T) {
		testMCPIncompleteJSONResponse(t, 250*time.Millisecond)
	})
}

func testMCPIncompleteJSONResponse(t *testing.T, deadline time.Duration) {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "echo"}, func(_ context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "complete"}}}, nil, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{JSONResponse: true})
	var calls atomic.Int32
	firstSession := make(chan string, 1)
	secondSession := make(chan string, 1)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			var request struct{ Method string }
			if err != nil || json.Unmarshal(body, &request) != nil {
				t.Error("could not read the MCP request")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if request.Method == "tools/call" {
				switch calls.Add(1) {
				case 1:
					firstSession <- r.Header.Get("Mcp-Session-Id")
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Content-Length", "100")
					_, _ = io.WriteString(w, `{"jsonrpc":"2.0",`)
					if deadline != 0 {
						w.(http.Flusher).Flush()
						<-r.Context().Done()
					}
					return
				case 2:
					secondSession <- r.Header.Get("Mcp-Session-Id")
				}
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(fixture.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	servers := NewServers(settings.MCPConfig{{Name: "fixture", URL: fixture.URL}}, fixture.Client(), nil)
	tools, release, err := servers.Open(ctx, "owner", []string{"fixture"})
	if err != nil || len(tools) != 1 {
		t.Fatalf("open fixture: %d tools, %v", len(tools), err)
	}
	defer func() { _ = release() }()
	callCtx := ctx
	if deadline != 0 {
		var stop context.CancelFunc
		callCtx, stop = context.WithTimeout(ctx, deadline)
		defer stop()
	}
	result, err := tools[0].Execute(callCtx, nil)
	if err == nil || result.Text != "" || ctx.Err() != nil {
		t.Fatalf("expected an isolated body read error: %+v, %v", result, err)
	}
	if deadline != 0 {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected a tool deadline: %v", err)
		}
	} else if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected a truncated response: %v", err)
	}
	if result, err := tools[0].Execute(ctx, nil); err != nil || result.Text != "complete" {
		t.Fatalf("next call after a truncated response: %+v, %v", result, err)
	}
	if first, second := <-firstSession, <-secondSession; first == "" || first != second || calls.Load() != 2 {
		t.Fatalf("expected one POST per call within the same session, got %d calls and session IDs %q and %q", calls.Load(), first, second)
	}
}

func TestMCPSessionCleanupReportsUnexpectedStatus(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusBadGateway} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
			sdk.AddTool(server, &sdk.Tool{Name: "echo"}, func(_ context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "complete"}}}, nil, nil
			})
			handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
			const privateDetail = "private upstream response"
			var deletions atomic.Int32
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					handler.ServeHTTP(w, r)
					return
				}
				if r.Header.Get("Mcp-Session-Id") == "" {
					t.Error("termination request did not identify the session")
				}
				deletions.Add(1)
				if status == http.StatusNoContent {
					handler.ServeHTTP(w, r)
				} else {
					http.Error(w, privateDetail, status)
				}
			}))
			t.Cleanup(fixture.Close)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			servers := NewServers(settings.MCPConfig{{Name: "fixture", URL: fixture.URL}}, fixture.Client(), nil)
			tools, release, err := servers.Open(ctx, "owner", []string{"fixture"})
			if err != nil || len(tools) != 1 {
				t.Fatalf("open fixture: %d tools, %v", len(tools), err)
			}
			result, err := tools[0].Execute(ctx, nil)
			if err != nil || result.Text != "complete" {
				t.Errorf("call tool: %+v, %v", result, err)
			}
			closeErr := release()
			if !errors.Is(release(), closeErr) || deletions.Load() != 1 {
				t.Fatal("releasing twice repeated the termination request or changed its result")
			}
			if status != http.StatusBadGateway {
				if closeErr != nil || strings.Contains(logs.String(), "MCP session cleanup failed") {
					t.Fatalf("expected cleanup outcome %d, got error %v and logs %q", status, closeErr, logs.String())
				}
				return
			}
			if code, ok := errors.AsType[sessionTerminationStatus](closeErr); !ok || int(code) != status {
				t.Fatalf("release did not report HTTP %d: %v", status, closeErr)
			}
			report := logs.String()
			for _, detail := range []string{"MCP session cleanup failed", "server=fixture", "HTTP 502"} {
				if !strings.Contains(report, detail) {
					t.Errorf("cleanup warning is missing %q: %s", detail, report)
				}
			}
			if strings.Contains(report, privateDetail) || strings.Contains(report, fixture.URL) || strings.Contains(closeErr.Error(), privateDetail) {
				t.Fatalf("cleanup exposed private upstream details: %v, %s", closeErr, report)
			}
		})
	}
}

func TestMCPRejectsOversizedToolResponse(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		json       bool
	}{
		{name: "JSON", json: true, want: errMCPResponseTooLarge.Error()},
		{name: "event stream", want: "SSE event exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
			sdk.AddTool(server, &sdk.Tool{Name: "report"}, func(_ context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
				text := strings.Repeat("x", maxMCPMessageBytes+1)
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}, nil, nil
			})
			fixture := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server },
				&sdk.StreamableHTTPOptions{JSONResponse: tc.json}))
			defer fixture.Close()
			servers := NewServers(settings.MCPConfig{{Name: "fixture", URL: fixture.URL}}, fixture.Client(), nil)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			tools, closeTools, err := servers.Open(ctx, "owner", []string{"fixture"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = closeTools() }()
			if len(tools) != 1 {
				t.Fatalf("loaded %d tools", len(tools))
			}
			result, err := tools[0].Execute(ctx, map[string]any{})
			if err == nil || !strings.Contains(err.Error(), tc.want) || ctx.Err() != nil || result.Text != "" {
				t.Fatalf("oversized response returned %d bytes of text, error=%v, context=%v", len(result.Text), err, ctx.Err())
			}
		})
	}
}

func TestMCPLimitRejectsAdvertisedOversizedBody(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(maxMCPMessageBytes+1))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer fixture.Close()
	client := *fixture.Client()
	client.Transport = responseLimitTransport{client.Transport}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fixture.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	if !errors.Is(err, errMCPResponseTooLarge) {
		t.Fatalf("oversized Content-Length: %v", err)
	}
}

func TestSelectedMCPServersExposeSameNamedToolsIndependently(t *testing.T) {
	mux := http.NewServeMux()
	names := []string{"first", "second"}
	ready := make(chan struct{})
	var connecting atomic.Int32
	for _, name := range names {
		server := sdk.NewServer(&sdk.Implementation{Name: name, Version: "1"}, nil)
		sdk.AddTool(server, &sdk.Tool{Name: "echo"}, func(_ context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: name}}}, nil, nil
		})
		handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
		var entered atomic.Bool
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
			if entered.CompareAndSwap(false, true) {
				if connecting.Add(1) == int32(len(names)) {
					close(ready)
				}
				select {
				case <-ready:
				case <-r.Context().Done():
					return
				}
			}
			handler.ServeHTTP(w, r)
		})
	}
	fixture := httptest.NewServer(mux)
	t.Cleanup(fixture.Close)
	servers := NewServers(settings.MCPConfig{
		{Name: "first", URL: fixture.URL + "/first"},
		{Name: "second", URL: fixture.URL + "/second"},
	}, fixture.Client(), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tools, closeTools, err := servers.Open(ctx, "owner", names)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeTools(); err != nil {
			t.Error(err)
		}
	})
	if len(tools) != len(names) {
		t.Fatalf("loaded %d tools, want %d", len(tools), len(names))
	}
	qualified := make([]string, len(tools))
	for i, tool := range tools {
		qualified[i] = tool.Name
		result, err := tool.Execute(t.Context(), map[string]any{})
		if err != nil || result.Text != names[i] {
			t.Fatalf("call %q: %+v, %v", tool.Name, result, err)
		}
	}
	if !slices.Equal(qualified, []string{qualifiedToolName("first", "echo"), qualifiedToolName("second", "echo")}) {
		t.Fatalf("ambiguous tool names: %v", qualified)
	}
	reordered, closeReordered, err := servers.Open(ctx, "owner", []string{"second", "first"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeReordered() }()
	if len(reordered) != 2 || reordered[0].Name != qualified[1] || reordered[1].Name != qualified[0] {
		t.Fatalf("reordering changed tool names: %+v", reordered)
	}
	single, closeSingle, err := servers.Open(ctx, "owner", []string{"second"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeSingle() }()
	if len(single) != 1 || single[0].Name != qualified[1] {
		t.Fatalf("selecting one server changed its tool name: %+v", single)
	}
}

func TestMCPListingFailureCancelsOtherStartupAndClosesBothSessions(t *testing.T) {
	const failing, slow = "failing", "slow"
	startupCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	names := []string{failing, slow}
	mux := http.NewServeMux()
	slowListing := make(chan struct{})
	allDeleting := make(chan struct{})
	var deleting atomic.Int32
	for _, name := range names {
		server := sdk.NewServer(&sdk.Implementation{Name: name, Version: "1"}, nil)
		server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
			return func(ctx context.Context, method string, request sdk.Request) (sdk.Result, error) {
				if method != "tools/list" {
					return next(ctx, method, request)
				}
				if name == failing {
					select {
					case <-slowListing:
						return nil, errors.New("fixture catalog unavailable")
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-startupCtx.Done():
						return nil, startupCtx.Err()
					}
				}
				close(slowListing)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-startupCtx.Done():
					return nil, startupCtx.Err()
				case <-allDeleting:
					return nil, errors.New("fixture session closed")
				}
			}
		})
		handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				if deleting.Add(1) == int32(len(names)) {
					close(allDeleting)
				}
				select {
				case <-r.Context().Done():
					return
				case <-allDeleting:
				}
			}
			handler.ServeHTTP(w, r)
		})
	}
	fixture := httptest.NewServer(mux)
	t.Cleanup(fixture.Close)
	servers := NewServers(settings.MCPConfig{
		{Name: failing, URL: fixture.URL + "/" + failing},
		{Name: slow, URL: fixture.URL + "/" + slow},
	}, fixture.Client(), nil)
	tools, release, err := servers.Open(startupCtx, "owner", names)
	if startupCtx.Err() != nil || err == nil || !strings.Contains(err.Error(), "fixture catalog unavailable") || release != nil || len(tools) != 0 {
		t.Fatalf("startup failed to return the listing error before its deadline: tools=%d release=%t error=%v context=%v",
			len(tools), release != nil, err, startupCtx.Err())
	}
	if deleting.Load() != int32(len(names)) {
		t.Fatalf("started %d session deletions, want %d", deleting.Load(), len(names))
	}
}
