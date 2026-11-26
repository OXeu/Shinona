package pigo

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

func withToolTimeout(tool agent.AgentTool) agent.AgentTool {
	schema := tool.Parameters
	if schema == nil || schema.Type != "object" || schema.Properties["timeout"] != nil {
		return tool
	}
	shape := schema.Clone()
	if shape.Properties == nil {
		shape.Properties = make(map[string]*ai.Schema)
	}
	timeout := ai.Integer("Optional maximum execution time in seconds. Omit for no tool deadline.")
	timeout.Minimum = new(1.0)
	shape.Properties["timeout"] = timeout
	shape.PropertyOrder = append(shape.PropertyOrder, "timeout")
	tool.Parameters = shape
	execute := tool.Execute
	tool.Execute = func(ctx context.Context, id string, args map[string]any, update agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		value, exists := args["timeout"]
		if !exists {
			return execute(ctx, id, args, update)
		}
		raw, err := json.Marshal(value)
		var seconds int64
		if err != nil || json.Unmarshal(raw, &seconds) != nil || seconds <= 0 || seconds > math.MaxInt64/int64(time.Second) {
			return agent.AgentToolResult{}, errors.New("timeout must be a positive integer in seconds")
		}
		arguments := maps.Clone(args)
		delete(arguments, "timeout")
		ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		defer cancel()
		return execute(ctx, id, arguments, update)
	}
	return tool
}
