// Package mcp connects configured Streamable HTTP servers to the agent's tool set.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"

	"github.com/OXeu/shinona/internal/settings"
)

type Servers struct {
	entries map[string]settings.MCPServer
	client  *http.Client
	oauth   *OAuth
}

func NewServers(config settings.MCPConfig, client *http.Client, oauth *OAuth) *Servers {
	servers := &Servers{entries: make(map[string]settings.MCPServer, len(config)), client: client, oauth: oauth}
	for _, entry := range config {
		servers.entries[entry.Name] = entry
	}
	return servers
}

func (s *Servers) Names(ctx context.Context, owner string) []string {
	names := make([]string, 0, len(s.entries))
	for name, server := range s.entries {
		if server.OAuth != nil && (s.oauth == nil || !s.oauth.Connected(ctx, owner, server)) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (s *Servers) Lookup(name string) (settings.MCPServer, bool) {
	server, ok := s.entries[name]
	return server, ok
}

func (s *Servers) Open(ctx context.Context, owner string, names []string) ([]agent.AgentTool, func() error, error) {
	var allTools []agent.AgentTool
	var sessions []func() error
	closeTools := func() error {
		var err error
		for _, closeSession := range slices.Backward(sessions) {
			err = errors.Join(err, closeSession())
		}
		return err
	}
	for i, name := range names {
		prefix, source := "mcp_", ""
		if len(names) > 1 {
			// A distinct prefix keeps identically named tools callable on each server.
			prefix, source = fmt.Sprintf("m%d_", i+1), fmt.Sprintf("MCP server %s. ", name)
		}
		tools, closeSession, err := s.open(ctx, owner, name, prefix, source)
		if err != nil {
			return nil, nil, errors.Join(fmt.Errorf("open MCP server %q: %w", name, err), closeTools())
		}
		sessions = append(sessions, closeSession)
		allTools = append(allTools, tools...)
	}
	return allTools, closeTools, nil
}

func (s *Servers) open(ctx context.Context, owner, name, prefix, source string) ([]agent.AgentTool, func() error, error) {
	server, ok := s.Lookup(name)
	if !ok {
		return nil, nil, errors.New("the selected MCP server is unavailable")
	}
	client := *s.client
	client.Timeout = 0
	if server.OAuth != nil {
		if s.oauth == nil || !s.oauth.Connected(ctx, owner, server) {
			return nil, nil, ErrOAuthRequired
		}
		client.Transport = s.oauth.transport(owner, server, oauthHTTP(server.URL).Transport)
	} else {
		client.Transport = tokenTransport{base: client.Transport, token: server.Token}
	}
	connection, err := sdk.NewClient(&sdk.Implementation{Name: "Shinona", Version: "1"}, nil).Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint: server.URL, HTTPClient: &client, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		return nil, nil, err
	}
	tools, err := listTools(ctx, connection, prefix, source)
	if err != nil {
		return nil, nil, errors.Join(err, connection.Close())
	}
	return tools, connection.Close, nil
}

type tokenTransport struct {
	base  http.RoundTripper
	token string
}

func (t tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,60}$`)

func listTools(ctx context.Context, connection *sdk.ClientSession, prefix, source string) ([]agent.AgentTool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tools := make([]agent.AgentTool, 0)
	seen := make(map[string]bool)
	for entry, err := range connection.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		schema, ok := entry.InputSchema.(map[string]any)
		if !ok || !toolName.MatchString(entry.Name) || seen[entry.Name] {
			continue
		}
		seen[entry.Name] = true
		raw, err := json.Marshal(schema)
		if err != nil {
			return nil, fmt.Errorf("encode MCP tool %q schema: %w", entry.Name, err)
		}
		var parameters ai.Schema
		if err := json.Unmarshal(raw, &parameters); err != nil {
			return nil, fmt.Errorf("decode MCP tool %q schema: %w", entry.Name, err)
		}
		tools = append(tools, agent.AgentTool{
			Name: prefix + entry.Name, Label: prefix + entry.Name, Description: source + entry.Description,
			Parameters: &parameters, Execute: executeTool(connection, entry.Name),
		})
		if len(tools) == 64 {
			break
		}
	}
	return tools, nil
}

func executeTool(session *sdk.ClientSession, name string) func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
	return func(ctx context.Context, _ string, args map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		var text strings.Builder
		for _, content := range result.Content {
			if item, ok := content.(*sdk.TextContent); ok {
				text.WriteString(item.Text[:min(len(item.Text), (16<<10)-text.Len())])
			}
		}
		if text.Len() == 0 && result.StructuredContent != nil {
			data, err := json.Marshal(result.StructuredContent)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			text.Write(data[:min(len(data), 16<<10)])
		}
		return agent.AgentToolResult{
			Content: ai.ContentList{ai.TextContent{Text: strings.ToValidUTF8(text.String(), "�")}}, IsError: result.IsError,
		}, nil
	}
}
