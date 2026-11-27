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
		duration, ok := toolTimeout(value)
		if !ok {
			return agent.AgentToolResult{}, errors.New("timeout must be a positive integer in seconds")
		}
		arguments := maps.Clone(args)
		delete(arguments, "timeout")
		ctx, cancel := context.WithTimeout(ctx, duration)
		defer cancel()
		return execute(ctx, id, arguments, update)
	}
	return tool
}

func toolTimeout(value any) (time.Duration, bool) {
	const maxSeconds = math.MaxInt64 / int64(time.Second)
	var seconds int64
	switch number := value.(type) {
	case float64:
		if !(number >= 1 && number <= float64(maxSeconds)) || math.Trunc(number) != number {
			return 0, false
		}
		seconds = int64(number)
	case int:
		seconds = int64(number)
	case int64:
		seconds = number
	case json.Number:
		var err error
		seconds, err = number.Int64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if seconds <= 0 || seconds > maxSeconds {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}
