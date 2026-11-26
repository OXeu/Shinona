package mcp

import (
	"context"
	"errors"
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
