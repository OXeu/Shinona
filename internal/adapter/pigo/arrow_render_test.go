package pigo

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/guanshan/pi-go/packages/ai"

	"github.com/OXeu/shinona/internal/run"
)

func TestArrowJSOnlyRunsWhenSelected(t *testing.T) {
	const source = "export default html`<strong>42</strong>`"
	model, ok := ai.Find(ai.BuiltinModels(), "faux", "faux")
	if !ok {
		t.Fatal("faux model missing")
	}
	for _, enabled := range []bool{false, true} {
		ai.SetFauxResponses([]ai.FauxResponse{
			{Content: []ai.ContentBlock{ai.FauxToolCall("display", "render_ui", map[string]string{"source": source, "summary": "Total is 42."})}},
			ai.NewFauxText("Here is the total."),
		})
		t.Cleanup(ai.ResetFauxResponses)
		var reported []run.TurnPart
		result, err := (&Agent{model: model, apiKey: "test"}).Run(t.Context(), run.Input{
			Prompt:      "Show the total.",
			Payload:     run.Payload{Options: run.Options{Permission: run.ReadOnly, ArrowJS: enabled}},
			ReportParts: func(parts []run.TurnPart) { reported = parts },
		}, &recordingWorkspace{})
		if err != nil {
			t.Fatal(err)
		}
		isDisplay := func(part run.TurnPart) bool {
			var files map[string]string
			return part.Kind == "ui" && part.Tool == "arrow-js" && part.Text == "Total is 42." &&
				json.Unmarshal([]byte(part.Output), &files) == nil && files["main.js"] == source
		}
		if slices.ContainsFunc(result.Parts, isDisplay) != enabled || slices.ContainsFunc(reported, isDisplay) != enabled {
			t.Fatalf("plugin enabled=%t: persisted parts %+v, live parts %+v", enabled, result.Parts, reported)
		}
	}
}

func TestArrowJSRejectsOversizedSource(t *testing.T) {
	result, err := arrowRenderTool().Execute(t.Context(), []byte(`{"summary":"Preview","source":"`+strings.Repeat("x", (16<<10)+1)+`"}`), nil)
	if err != nil || !result.IsError {
		t.Fatalf("expected oversized source to fail, got %+v, %v", result, err)
	}
}
