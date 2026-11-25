package pigo

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"time"

	"github.com/guanshan/pi-go/packages/agent"
)

type timeoutTool struct {
	agent.AgentTool
	schema map[string]any
}

func withToolTimeout(tool agent.AgentTool) agent.AgentTool {
	schema := tool.Schema()
	if value, exists := schema["type"]; exists {
		if kind, _ := value.(string); kind != "object" {
			return tool
		}
	}
	var properties map[string]any
	if object, exists := schema["properties"]; exists {
		var ok bool
		properties, ok = object.(map[string]any)
		if !ok {
			return tool
		}
	}
	if _, native := properties["timeout"]; native {
		return tool
	}
	fields := make(map[string]any, len(properties)+1)
	maps.Copy(fields, properties)
	fields["timeout"] = map[string]any{
		"type": "integer", "minimum": 1, "description": "Optional maximum execution time in seconds. Omit for no tool deadline.",
	}
	shape := maps.Clone(schema)
	if shape == nil {
		shape = map[string]any{"type": "object"}
	}
	shape["properties"] = fields
	return timeoutTool{AgentTool: tool, schema: shape}
}

func (t timeoutTool) Schema() map[string]any { return t.schema }

func (t timeoutTool) Execute(ctx context.Context, args json.RawMessage, update agent.ToolUpdateCallback) (agent.AgentToolResult, error) {
	if len(args) == 0 {
		return t.AgentTool.Execute(ctx, args, update)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(args, &values); err != nil {
		return agent.AgentToolResult{}, err
	}
	value, exists := values["timeout"]
	if !exists {
		return t.AgentTool.Execute(ctx, args, update)
	}
	var seconds int64
	if err := json.Unmarshal(value, &seconds); err != nil || seconds <= 0 || seconds > math.MaxInt64/int64(time.Second) {
		return agent.AgentToolResult{}, errors.New("timeout must be a positive integer in seconds")
	}
	delete(values, "timeout")
	arguments, err := json.Marshal(values)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	return t.AgentTool.Execute(ctx, arguments, update)
}
