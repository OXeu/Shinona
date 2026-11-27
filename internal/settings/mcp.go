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
	Name                 string `json:"name"`
	URL                  string `json:"url"`
	HasToken             bool   `json:"hasToken"`
	OAuth                bool   `json:"oauth"`
	OAuthClientID        string `json:"oauthClientId,omitempty"`
	HasOAuthClientSecret bool   `json:"hasOAuthClientSecret,omitempty"`
	Connected            bool   `json:"connected"`
}

type MCPSettings struct {
	Servers    []MCPServerInfo `json:"servers"`
	Overridden bool            `json:"overridden"`
}

type MCPServerInput struct {
	PreviousName      *string
	Name              string
	URL               string
	Token             *string // nil retains the saved token, and an empty string clears it.
	OAuth             *bool
	OAuthClientID     *string `json:"oauthClientId"`     // An empty string enables automatic registration.
	OAuthClientSecret *string `json:"oauthClientSecret"` // nil retains the secret for the same endpoint and client ID.
}

func (servers MCPConfig) Info(overridden bool) MCPSettings {
	result := MCPSettings{Servers: make([]MCPServerInfo, len(servers)), Overridden: overridden}
	for i, server := range servers {
		info := MCPServerInfo{Name: server.Name, URL: server.URL, HasToken: server.Token != "", OAuth: server.OAuth != nil}
		if server.OAuth != nil {
			info.OAuthClientID = server.OAuth.ClientID
			info.HasOAuthClientSecret = server.OAuth.ClientSecret != ""
		}
		result.Servers[i] = info
	}
	slices.SortFunc(result.Servers, func(a, b MCPServerInfo) int { return strings.Compare(a.Name, b.Name) })
	return result
}

func (servers MCPConfig) Save(input MCPServerInput) (MCPConfig, error) {
	if strings.ContainsAny(input.Name, "\x00\r\n") || strings.ContainsAny(input.URL, "\x00\r\n") {
		return nil, Invalid("INVALID_MCP_SERVERS", "the MCP server name or endpoint contains an invalid character")
	}
	entry := MCPServer{Name: strings.TrimSpace(input.Name), URL: strings.TrimSpace(input.URL)}
	if input.Token != nil {
		entry.Token = *input.Token
	}
	oauth := input.OAuth != nil && *input.OAuth
	if !oauth && (input.OAuthClientID != nil || input.OAuthClientSecret != nil) {
		return nil, Invalid("INVALID_MCP_OAUTH", "enable OAuth to configure an OAuth client")
	}
	index := -1
	var previousClient *MCPOAuthConfig
	if input.PreviousName != nil {
		index = servers.index(*input.PreviousName)
		if index < 0 {
			return nil, mcpServerNotFound()
		}
		saved := servers[index]
		if input.Token == nil && !oauth {
			if saved.Token != "" && saved.URL != entry.URL {
				return nil, Invalid("MCP_TOKEN_REQUIRED_FOR_URL_CHANGE", "enter a new token or select no authentication when changing the endpoint")
			}
			entry.Token = saved.Token
		}
		if saved.URL == entry.URL {
			previousClient = saved.OAuth
		}
	}
	if oauth {
		client := input.oauthClient(previousClient)
		entry.OAuth = &client
	}
	next := slices.Clone(servers)
	if index < 0 {
		next = append(next, entry)
	} else {
		next[index] = entry
	}
	return next, next.Validate()
}

func (input MCPServerInput) oauthClient(previous *MCPOAuthConfig) MCPOAuthConfig {
	var client MCPOAuthConfig
	if previous != nil {
		client.ClientID = previous.ClientID
	}
	if input.OAuthClientID != nil {
		client.ClientID = *input.OAuthClientID
		if !strings.ContainsAny(client.ClientID, "\x00\r\n") {
			client.ClientID = strings.TrimSpace(client.ClientID)
		}
	}
	if input.OAuthClientSecret != nil {
		client.ClientSecret = *input.OAuthClientSecret
	} else if previous != nil && previous.ClientID == client.ClientID {
		client.ClientSecret = previous.ClientSecret
	}
	return client
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
		if len(entry.URL) > 2048 || len(entry.Token) > 4096 {
			return Invalid("MCP_SERVER_FIELD_LIMIT", "MCP URLs are limited to 2048 bytes and tokens to 4096 bytes")
		}
		target, err := url.Parse(entry.URL)
		if !mcpconfig.ValidName(entry.Name) ||
			err != nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" ||
			target.ForceQuery || target.Fragment != "" || (target.Scheme != "https" && target.Scheme != "http") ||
			strings.ContainsAny(target.Path, "\x00\r\n") || strings.ContainsAny(entry.Token, "\x00\r\n") {
			return Invalid("INVALID_MCP_SERVERS", "enter a name and an HTTP(S) MCP endpoint without embedded credentials, a query, or a fragment")
		}
		if names[entry.Name] {
			return Invalid("MCP_SERVER_DUPLICATE", "MCP server names must be unique")
		}
		if client := entry.OAuth; client != nil {
			if entry.Token != "" {
				return Invalid("INVALID_MCP_OAUTH", "choose OAuth without a bearer token")
			}
			if len(client.ClientID) > 2048 || len(client.ClientSecret) > 4096 ||
				strings.ContainsAny(client.ClientID, "\x00\r\n") || strings.ContainsAny(client.ClientSecret, "\x00\r\n") ||
				(client.ClientSecret != "" && strings.TrimSpace(client.ClientID) == "") {
				return Invalid("INVALID_MCP_OAUTH_CLIENT", "enter a valid client ID for the OAuth client secret")
			}
		}
		names[entry.Name] = true
	}
	return nil
}
