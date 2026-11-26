package settings

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/OXeu/shinona/internal/mcpconfig"
)

type MCPServer struct {
	Name  string          `json:"name"`
	URL   string          `json:"url"`
	Token string          `json:"token,omitempty"`
	OAuth *MCPOAuthConfig `json:"oauth,omitempty"`
}

type MCPOAuthConfig struct {
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

type MCPConfig []MCPServer

type MCPRepository interface {
	MCPConfig(context.Context, string) (MCPConfig, error)
	SaveMCPConfig(context.Context, string, MCPConfig, []string) error
}

type MCPServerInfo struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	HasToken  bool   `json:"hasToken"`
	OAuth     bool   `json:"oauth"`
	Connected bool   `json:"connected"`
}

type MCPSettings struct {
	Servers    []MCPServerInfo `json:"servers"`
	Overridden bool            `json:"overridden"`
}

type MCPServerInput struct {
	PreviousName *string
	Name         string
	URL          string
	Token        *string // nil retains the saved token, and an empty string clears it.
	OAuth        *bool
}

func (servers MCPConfig) Info(overridden bool) MCPSettings {
	result := MCPSettings{Servers: make([]MCPServerInfo, len(servers)), Overridden: overridden}
	for i, server := range servers {
		result.Servers[i] = MCPServerInfo{Name: server.Name, URL: server.URL, HasToken: server.Token != "", OAuth: server.OAuth != nil}
	}
	slices.SortFunc(result.Servers, func(a, b MCPServerInfo) int { return strings.Compare(a.Name, b.Name) })
	return result
}

func (servers MCPConfig) Save(input MCPServerInput) (MCPConfig, error) {
	entry := MCPServer{Name: strings.TrimSpace(input.Name), URL: strings.TrimSpace(input.URL)}
	if len(entry.URL) > 2048 || input.Token != nil && len(*input.Token) > 4096 {
		return nil, Invalid("MCP_SERVER_FIELD_LIMIT", "MCP URLs are limited to 2048 bytes and tokens to 4096 bytes")
	}
	if input.Token != nil {
		entry.Token = *input.Token
	}
	if input.OAuth != nil && *input.OAuth {
		entry.OAuth = &MCPOAuthConfig{}
	}
	if strings.ContainsAny(entry.Name, "\x00\r\n") || strings.ContainsRune(entry.Token, 0) {
		return nil, Invalid("INVALID_MCP_SERVERS", "the MCP server name or token contains an invalid character")
	}

	next := slices.Clone(servers)
	if input.PreviousName == nil {
		next = append(next, entry)
	} else {
		i := servers.index(*input.PreviousName)
		if i < 0 {
			return nil, mcpServerNotFound()
		}
		saved := servers[i]
		if input.Token == nil && entry.OAuth == nil {
			if saved.Token != "" && saved.URL != entry.URL {
				return nil, Invalid("MCP_TOKEN_REQUIRED_FOR_URL_CHANGE", "enter a new token or select no authentication when changing the endpoint")
			}
			entry.Token = saved.Token
		}
		if entry.OAuth != nil && saved.OAuth != nil && saved.URL == entry.URL {
			entry.OAuth = saved.OAuth
		}
		next[i] = entry
	}
	return next, next.Validate()
}

func (servers MCPConfig) Remove(name string) (MCPConfig, error) {
	i := servers.index(name)
	if i < 0 {
		return nil, mcpServerNotFound()
	}
	return slices.Delete(slices.Clone(servers), i, i+1), nil
}

func (servers MCPConfig) index(name string) int {
	return slices.IndexFunc(servers, func(server MCPServer) bool { return server.Name == name })
}

func mcpServerNotFound() error {
	return Invalid("MCP_SERVER_NOT_FOUND", "the MCP server no longer exists")
}

func (servers MCPConfig) Validate() error {
	if len(servers) > mcpconfig.MaxServers {
		return Invalid("MCP_SERVER_LIMIT", fmt.Sprintf("configure at most %d MCP servers", mcpconfig.MaxServers))
	}
	names := make(map[string]bool, len(servers))
	for _, entry := range servers {
		target, err := url.Parse(entry.URL)
		if !mcpconfig.ValidName(entry.Name) ||
			err != nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" ||
			target.ForceQuery || target.Fragment != "" || (target.Scheme != "https" && target.Scheme != "http") ||
			strings.ContainsAny(entry.Token, "\r\n") {
			return Invalid("INVALID_MCP_SERVERS", "enter a name and an HTTP(S) MCP endpoint without embedded credentials, a query, or a fragment")
		}
		if names[entry.Name] {
			return Invalid("MCP_SERVER_DUPLICATE", "MCP server names must be unique")
		}
		if entry.OAuth != nil && (entry.Token != "" || len(entry.OAuth.ClientID) > 2048 ||
			len(entry.OAuth.ClientSecret) > 4096 || strings.ContainsAny(entry.OAuth.ClientID+entry.OAuth.ClientSecret, "\x00\r\n") ||
			(entry.OAuth.ClientSecret != "" && entry.OAuth.ClientID == "")) {
			return Invalid("INVALID_MCP_OAUTH", "choose OAuth without a bearer token")
		}
		names[entry.Name] = true
	}
	return nil
}
