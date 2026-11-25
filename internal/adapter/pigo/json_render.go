package pigo

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/guanshan/pi-go/packages/agent"
)

//go:embed json_render.schema.json
var displaySchemaJSON []byte

var displaySchema = func() *jsonschema.Resolved {
	var schema jsonschema.Schema
	if err := json.Unmarshal(displaySchemaJSON, &schema); err != nil {
		panic(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		panic(err)
	}
	return resolved
}()

type renderedUI struct {
	spec    string
	summary string
}

func jsonRenderTool() agent.AgentTool {
	return tool{
		name:        "render_ui",
		description: `Show structured results to the user as a UI. Call when data benefits from a table, metrics, progress or grouped cards. Provide a concise plain-text summary and a JSON string spec with root and elements. Each element has type, props, and optionally children (element IDs). Components and props: Stack {}; Card {title,description?}; Text {text}; Metric {label,value,detail?}; Badge {text,tone?} (neutral, success, warning, danger); List {items:string[]}; Table {columns:string[],rows:string[][]}; Progress {label,value} (0–100). Only Stack and Card have children. Use each element once. Example spec: {"root":"total","elements":{"total":{"type":"Metric","props":{"label":"Total","value":"42"}}}}. The user sees the UI directly; do not repeat the JSON in your answer.`,
		fields:      []string{"spec", "summary"},
		execute: func(_ context.Context, args map[string]string) (agent.AgentToolResult, error) {
			summary := strings.TrimSpace(args["summary"])
			if summary == "" || utf8.RuneCountInString(summary) > 320 {
				return toolFailure("Provide a plain-text summary of 1 to 320 characters."), nil
			}
			spec, err := validateDisplaySpec(args["spec"])
			if err != nil {
				return toolFailure("Invalid UI spec: " + clipPart(err.Error(), 512)), nil
			}
			result := textResult("Displayed the UI to the user.")
			result.Details = renderedUI{spec, summary}
			return result, nil
		},
	}
}

func validateDisplaySpec(input string) (string, error) {
	if len(input) > partBytes {
		return "", fmt.Errorf("keep the display under %d bytes", partBytes)
	}
	var value any
	if err := json.Unmarshal([]byte(input), &value); err != nil {
		return "", err
	}
	if err := displaySchema.Validate(value); err != nil {
		return "", err
	}
	var tree struct {
		Root     string
		Elements map[string]struct{ Children []string }
	}
	if err := json.Unmarshal([]byte(input), &tree); err != nil {
		return "", err
	}
	visited := make(map[string]bool, len(tree.Elements))
	var visit func(string, int) bool
	visit = func(id string, depth int) bool {
		element, exists := tree.Elements[id]
		if !exists || visited[id] || depth > 12 {
			return false
		}
		visited[id] = true
		for _, child := range element.Children {
			if !visit(child, depth+1) {
				return false
			}
		}
		return true
	}
	if !visit(tree.Root, 1) || len(visited) != len(tree.Elements) {
		return "", fmt.Errorf("elements must form one tree with depth at most 12")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(input)); err != nil {
		return "", err
	}
	return compact.String(), nil
}
