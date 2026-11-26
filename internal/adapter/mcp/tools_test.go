package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sky-valley/pi/ai"

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
	if len(tools) != 1 || tools[0].Name != "mcp_echo" {
		t.Fatalf("unexpected tools: %v", tools)
	}
	result, err := tools[0].Execute(t.Context(), "fixture-call", map[string]any{"text": "hello"}, nil)
	if err != nil || len(result.Content) != 1 || ai.ContentText(result.Content) != "hello" {
		t.Fatalf("call tool: %+v, %v", result, err)
	}
}

func TestSelectedMCPServersExposeSameNamedToolsIndependently(t *testing.T) {
	mux := http.NewServeMux()
	names := []string{"first", "second"}
	for _, name := range names {
		server := sdk.NewServer(&sdk.Implementation{Name: name, Version: "1"}, nil)
		sdk.AddTool(server, &sdk.Tool{Name: "echo"}, func(_ context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: name}}}, nil, nil
		})
		handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
		mux.Handle("/"+name, handler)
	}
	fixture := httptest.NewServer(mux)
	t.Cleanup(fixture.Close)
	servers := NewServers(settings.MCPConfig{
		{Name: "first", URL: fixture.URL + "/first"},
		{Name: "second", URL: fixture.URL + "/second"},
	}, fixture.Client(), nil)
	tools, closeTools, err := servers.Open(t.Context(), "owner", names)
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
		result, err := tool.Execute(t.Context(), "fixture-call", map[string]any{}, nil)
		if err != nil || len(result.Content) != 1 || ai.ContentText(result.Content) != names[i] {
			t.Fatalf("call %q: %+v, %v", tool.Name, result, err)
		}
	}
	if !slices.Equal(qualified, []string{"m1_echo", "m2_echo"}) {
		t.Fatalf("ambiguous tool names: %v", qualified)
	}
}
