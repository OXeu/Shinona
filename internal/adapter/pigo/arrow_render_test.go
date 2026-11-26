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
			Prompt:  "Show the total.",
			Payload: run.Payload{Options: run.Options{Permission: run.ReadOnly, ArrowJS: enabled}},
			ReportParts: func(_ int, parts []run.TurnPart) {
				reportedDisplay = reportedDisplay || slices.ContainsFunc(parts, isDisplay)
			},
		}, &recordingWorkspace{})
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(result.Parts, isDisplay) != enabled || reportedDisplay != enabled {
			t.Fatalf("plugin enabled=%t: persisted parts %+v, rendered live=%t", enabled, result.Parts, reportedDisplay)
		}
	}
}

func TestArrowJSLimits(t *testing.T) {
	source := strings.Repeat("é", 8192)
	css := strings.Repeat("é", 4096)
	summary := strings.Repeat("文", 320)
	for _, fixture := range []struct {
		name, source, css, summary string
		rejected                   bool
	}{
		{name: "exact limits", source: source, css: css, summary: summary},
		{name: "script size", source: source + "x", css: css, summary: summary, rejected: true},
		{name: "style size", source: source, css: css + "x", summary: summary, rejected: true},
		{name: "summary length", source: source, css: css, summary: summary + "文", rejected: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			result, err := arrowRenderTool().Execute(t.Context(), "display", map[string]any{
				"source": fixture.source, "css": fixture.css, "summary": fixture.summary,
			}, nil)
			if err != nil || result.IsError != fixture.rejected {
				t.Fatalf("tool returned isError=%v and error=%v", result.IsError, err)
			}
			if fixture.rejected {
				return
			}
			var files map[string]string
			details, ok := result.Details.(renderedUI)
			if !ok || details.summary != summary || json.Unmarshal([]byte(details.source), &files) != nil ||
				files["main.js"] != source || files["main.css"] != css {
				t.Fatal("the existing wire format did not preserve the full script, stylesheet, and summary")
			}
		})
	}
}
