package pigo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/guanshan/pi-go/packages/agent"
	"github.com/guanshan/pi-go/packages/ai"

	"github.com/OXeu/shinona/internal/sandbox"
)

type tool struct {
	name        string
	description string
	fields      []string
	execute     func(context.Context, map[string]string) (agent.AgentToolResult, error)
}

func (t tool) Name() string        { return t.name }
func (t tool) Label() string       { return t.name }
func (t tool) Description() string { return t.description }

func (t tool) Schema() map[string]any {
	properties := make(map[string]any, len(t.fields))
	for _, field := range t.fields {
		properties[field] = map[string]string{"type": "string"}
	}
	return map[string]any{
		"type": "object", "properties": properties, "required": t.fields, "additionalProperties": false,
	}
}

func (t tool) Execute(ctx context.Context, input json.RawMessage, _ agent.ToolUpdateCallback) (agent.AgentToolResult, error) {
	var args map[string]string
	if err := json.Unmarshal(input, &args); err != nil {
		return agent.AgentToolResult{}, err
	}
	for _, field := range t.fields {
		if _, found := args[field]; !found {
			return agent.AgentToolResult{}, fmt.Errorf("missing %q", field)
		}
	}
	return t.execute(ctx, args)
}

func tools(workspace sandbox.Workspace, writable bool) []agent.AgentTool {
	read := []agent.AgentTool{
		tool{name: "read", description: "Read a text file at a workspace-relative path.", fields: []string{"path"},
			execute: func(ctx context.Context, args map[string]string) (agent.AgentToolResult, error) {
				data, err := workspace.ReadFile(ctx, args["path"])
				return textResult(string(data)), err
			}},
	}
	if !writable {
		return read
	}
	return append(read,
		tool{name: "write", description: "Write text to a workspace-relative path.", fields: []string{"path", "content"},
			execute: func(ctx context.Context, args map[string]string) (agent.AgentToolResult, error) {
				err := workspace.WriteFile(ctx, args["path"], []byte(args["content"]))
				return textResult("File written."), err
			}},
		tool{name: "bash", description: "Run a Bash command inside the sandbox workspace.", fields: []string{"command"},
			execute: func(ctx context.Context, args map[string]string) (agent.AgentToolResult, error) {
				if args["command"] == "" {
					return agent.AgentToolResult{}, errors.New("command must be nonempty")
				}
				start := time.Now()
				result, err := workspace.Exec(ctx, sandbox.Command{Program: "/bin/bash", Args: []string{"-c", args["command"]}})
				if err != nil {
					text := fmt.Sprintf("Bash failed after %s without an exit code.\n%s%s",
						time.Since(start).Round(time.Millisecond), clipPart(err.Error(), 1024), failedCommandOutput(result))
					return toolFailure(text), nil
				}
				if result.ExitCode != 0 {
					text := fmt.Sprintf("Bash exited with code %d.", result.ExitCode)
					if result.ExitDetail != "" {
						text += " " + clipPart(result.ExitDetail, 1024)
					}
					return toolFailure(text + failedCommandOutput(result)), nil
				}
				text := fmt.Sprintf("Exit code: %d\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Stdout, result.Stderr)
				if result.Truncated {
					text += "\nOutput truncated."
				}
				return textResult(text), nil
			}},
	)
}

func failedCommandOutput(result sandbox.Result) string {
	// Keep both streams in the transcript preview. stderr often holds the cause.
	const streamPreview = 3 << 10
	var output strings.Builder
	if result.Stderr != "" {
		fmt.Fprintf(&output, "\nstderr:\n%s", clipPart(strings.ToValidUTF8(result.Stderr, "�"), streamPreview))
	}
	if result.Stdout != "" {
		fmt.Fprintf(&output, "\nstdout:\n%s", clipPart(strings.ToValidUTF8(result.Stdout, "�"), streamPreview))
	}
	if result.Truncated || len(result.Stderr) > streamPreview || len(result.Stdout) > streamPreview {
		output.WriteString("\nOutput truncated.")
	}
	return output.String()
}

func textResult(text string) agent.AgentToolResult {
	return agent.AgentToolResult{Content: ai.TextBlocks(text)}
}

func toolFailure(text string) agent.AgentToolResult {
	return agent.AgentToolResult{Content: ai.TextBlocks(text), IsError: true}
}
