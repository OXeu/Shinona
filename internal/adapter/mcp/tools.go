// Package mcp connects configured Streamable HTTP servers to the agent's tool set.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/errgroup"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
	"github.com/OXeu/shinona/internal/timing"
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

func (s *Servers) Open(ctx context.Context, owner string, names []string) ([]run.Tool, func() error, error) {
	type connection struct {
		tools []run.Tool
		close func() error
	}
	connections := make([]connection, len(names))
	closeTools := func() error {
		var err error
		for _, session := range slices.Backward(connections) {
			if session.close != nil {
				err = errors.Join(err, session.close())
			}
		}
		return err
	}
	startup, openCtx := errgroup.WithContext(ctx)
	startup.SetLimit(4)
	for i, name := range names {
		startup.Go(func() error {
			if err := openCtx.Err(); err != nil {
				return err
			}
			tools, closeSession, err := s.open(openCtx, owner, name)
			if err != nil {
				return fmt.Errorf("open MCP server %q: %w", name, err)
			}
			connections[i] = connection{tools, closeSession}
			return nil
		})
	}
	if err := startup.Wait(); err != nil {
		return nil, nil, errors.Join(err, closeTools())
	}
	var allTools []run.Tool
	for _, session := range connections {
		allTools = append(allTools, session.tools...)
	}
	return allTools, closeTools, nil
}

func (s *Servers) open(ctx context.Context, owner, name string) ([]run.Tool, func() error, error) {
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
	connect, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	initialized := timing.Start(ctx, "mcp_initialize")
	connection, err := sdk.NewClient(&sdk.Implementation{Name: "Shinona", Version: "1"}, nil).Connect(connect, &sdk.StreamableClientTransport{
		Endpoint: server.URL, HTTPClient: &client, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	initialized()
	if err != nil {
		return nil, nil, err
	}
	tools, err := listTools(ctx, connection, name)
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

func qualifiedToolName(server, name string) string {
	const maxName = 64
	id := sha256.Sum256([]byte(server))
	prefix := fmt.Sprintf("mcp_%x_", id[:8])
	if len(prefix)+len(name) <= maxName {
		return prefix + name
	}
	short := sha256.Sum256([]byte(name))
	suffix := fmt.Sprintf("_%x", short[:8])
	return prefix + name[:maxName-len(prefix)-len(suffix)] + suffix
}

func listTools(ctx context.Context, connection *sdk.ClientSession, server string) ([]run.Tool, error) {
	defer timing.Start(ctx, "mcp_list_tools")()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tools := make([]run.Tool, 0)
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
		tools = append(tools, run.Tool{
			Name: qualifiedToolName(server, entry.Name), Label: server + ": " + entry.Name,
			Description: fmt.Sprintf("MCP server %s. %s", server, entry.Description),
			Schema:      raw, Execute: executeTool(connection, entry.Name),
		})
		if len(tools) == 64 {
			break
		}
	}
	return tools, nil
}

func executeTool(session *sdk.ClientSession, name string) func(context.Context, map[string]any) (run.ToolResult, error) {
	return func(ctx context.Context, args map[string]any) (run.ToolResult, error) {
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return run.ToolResult{}, err
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
				return run.ToolResult{}, err
			}
			text.Write(data[:min(len(data), 16<<10)])
		}
		return run.ToolResult{
			Text: strings.ToValidUTF8(text.String(), "�"), IsError: result.IsError,
		}, nil
	}
}
