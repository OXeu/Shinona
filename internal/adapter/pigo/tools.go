package pigo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

func remoteAgentTool(remote run.Tool) (agent.AgentTool, error) {
	var parameters ai.Schema
	if err := json.Unmarshal(remote.Schema, &parameters); err != nil {
		return agent.AgentTool{}, fmt.Errorf("decode tool %q schema: %w", remote.Name, err)
	}
	return agent.AgentTool{
		Name: remote.Name, Label: remote.Label, Description: remote.Description, Parameters: &parameters,
		Execute: func(ctx context.Context, _ string, args map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			output, err := remote.Execute(ctx, args)
			result := textResult(output.Text)
			result.IsError = output.IsError
			return result, err
		},
	}, nil
}

func tools(workspace sandbox.Workspace, writable bool) []agent.AgentTool {
	read := []agent.AgentTool{{
		Name: "read", Label: "read", Description: "Read a text file at a workspace-relative path.",
		Parameters: toolParameters(ai.Prop("path", ai.String())),
		Execute: func(ctx context.Context, _ string, args map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			data, err := workspace.ReadFile(ctx, args["path"].(string))
			return textResult(string(data)), err
		},
	}}
	if !writable {
		return read
	}
	return append(read,
		agent.AgentTool{
			Name: "write", Label: "write", Description: "Write text to a workspace-relative path.",
			Parameters: toolParameters(ai.Prop("path", ai.String()), ai.Prop("content", ai.String())),
			Execute: func(ctx context.Context, _ string, args map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
				err := workspace.WriteFile(ctx, args["path"].(string), []byte(args["content"].(string)))
				return textResult("File written."), err
			},
		},
		agent.AgentTool{
			Name: "bash", Label: "bash", Description: "Run a shell command in the workspace. Uses Bash if available, or POSIX sh.",
			Parameters: toolParameters(ai.Prop("command", ai.String())),
			Execute: func(ctx context.Context, _ string, args map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
				command := args["command"].(string)
				if command == "" {
					return agent.AgentToolResult{}, errors.New("command must be nonempty")
				}
				start := time.Now()
				result, err := workspace.Exec(ctx, sandbox.Shell("-c", command))
				if err != nil {
					text := fmt.Sprintf("Command failed after %s without an exit code.\n%s%s",
						time.Since(start).Round(time.Millisecond), clipPart(err.Error(), 1024), failedCommandOutput(result))
					return toolFailure(text), nil
				}
				if result.ExitCode != 0 {
					text := fmt.Sprintf("Command exited with code %d.", result.ExitCode)
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
			},
		},
	)
}

func toolParameters(fields ...ai.Field) *ai.Schema {
	parameters := ai.Object(fields...)
	parameters.AdditionalAllowed = new(false)
	return parameters
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
	return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: text}}}
}

func toolFailure(text string) agent.AgentToolResult {
	return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: text}}, IsError: true}
}
