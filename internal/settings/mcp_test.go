package settings

import (
	"strings"
	"testing"
)

func TestMCPConfigRejectsInvalidFieldsOnSaveAndValidate(t *testing.T) {
	valid := MCPServer{Name: "server", URL: "https://example.test/mcp", Token: "token"}
	for _, entry := range []MCPServer{
		{Name: valid.Name + "\r\n", URL: valid.URL, Token: valid.Token},
		{Name: valid.Name, URL: valid.URL + "\n", Token: valid.Token},
		{Name: valid.Name, URL: valid.URL + "%00", Token: valid.Token},
		{Name: valid.Name, URL: valid.URL + strings.Repeat("x", 2048), Token: valid.Token},
		{Name: valid.Name, URL: valid.URL, Token: valid.Token + "\x00"},
		{Name: valid.Name, URL: valid.URL, Token: valid.Token + "\r\n"},
		{Name: valid.Name, URL: valid.URL, Token: strings.Repeat("é", 2049)},
	} {
		if err := (MCPConfig{entry}).Validate(); err == nil {
			t.Error("Validate accepted an invalid field")
		}
		input := MCPServerInput{Name: entry.Name, URL: entry.URL, Token: &entry.Token}
		if _, err := (MCPConfig{}).Save(input); err == nil {
			t.Error("Save accepted an invalid field")
		}
	}
}
