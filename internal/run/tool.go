package run

import (
	"context"
	"encoding/json"
)

// Tool is a remote tool definition independent of the agent SDK.
type Tool struct {
	Name, Label, Description string
	Schema                   json.RawMessage
	Execute                  func(context.Context, map[string]any) (ToolResult, error)
}

type ToolResult struct {
	Text    string
	IsError bool
}
