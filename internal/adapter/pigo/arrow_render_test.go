package pigo

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"

	"github.com/OXeu/shinona/internal/run"
)

func TestArrowJSOnlyRunsWhenSelected(t *testing.T) {
	const source = "export default html`<strong>42</strong>`"
	for _, enabled := range []bool{false, true} {
		fixture := scriptedAgent(t,
			ai.AssistantMessage{StopReason: ai.StopToolUse, Content: ai.ContentList{
				ai.ToolCall{ID: "display", Name: "render_ui", Arguments: map[string]any{"source": source, "summary": "Total is 42."}},
			}},
			ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "Here is the total."}}},
		)
		isDisplay := func(part run.TurnPart) bool {
			var files map[string]string
			return part.Kind == "ui" && part.Tool == "arrow-js" && part.Text == "Total is 42." &&
				json.Unmarshal([]byte(part.Output), &files) == nil && files["main.js"] == source
		}
		var reportedDisplay bool
		result, err := fixture.Run(t.Context(), run.Input{
			Prompt:      "Show the total.",
			Payload:     run.Payload{Options: run.Options{Permission: run.ReadOnly, ArrowJS: enabled}},
			ReportParts: func(parts []run.TurnPart) { reportedDisplay = reportedDisplay || slices.ContainsFunc(parts, isDisplay) },
		}, &recordingWorkspace{})
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(result.Parts, isDisplay) != enabled || reportedDisplay != enabled {
			t.Fatalf("plugin enabled=%t: persisted parts %+v, rendered live=%t", enabled, result.Parts, reportedDisplay)
		}
	}
}

func TestArrowJSRejectsOversizedSource(t *testing.T) {
	result, err := arrowRenderTool().Execute(t.Context(), "display", map[string]any{
		"summary": "Preview", "source": strings.Repeat("x", (16<<10)+1),
	}, nil)
	if err != nil || !result.IsError {
		t.Fatalf("expected oversized source to fail, got %+v, %v", result, err)
	}
}
