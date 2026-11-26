package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/OXeu/shinona/internal/settings"
)

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
