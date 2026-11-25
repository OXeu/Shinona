package pigo

import (
	"slices"
	"strings"
	"testing"

	"github.com/guanshan/pi-go/packages/ai"

	"github.com/OXeu/shinona/internal/run"
)

const metricSpec = `{"root":"total","elements":{"total":{"type":"Metric","props":{"label":"Total","value":"42"}}}}`

func TestJSONRenderOnlyRunsWhenSelected(t *testing.T) {
	model, ok := ai.Find(ai.BuiltinModels(), "faux", "faux")
	if !ok {
		t.Fatal("faux model missing")
	}
	for _, enabled := range []bool{false, true} {
		ai.SetFauxResponses([]ai.FauxResponse{
			{Content: []ai.ContentBlock{ai.FauxToolCall("display", "render_ui", map[string]string{"spec": metricSpec, "summary": "Total is 42."})}},
			ai.NewFauxText("Here is the total."),
		})
		t.Cleanup(ai.ResetFauxResponses)
		var reported []run.TurnPart
		result, err := (&Agent{model: model, apiKey: "test"}).Run(t.Context(), run.Input{
			Prompt:      "Show the total.",
			Payload:     run.Payload{Options: run.Options{Permission: run.ReadOnly, JSONRender: enabled}},
			ReportParts: func(parts []run.TurnPart) { reported = parts },
		}, &recordingWorkspace{})
		if err != nil {
			t.Fatal(err)
		}
		isDisplay := func(part run.TurnPart) bool {
			return part.Kind == "ui" && part.Output == metricSpec && part.Text == "Total is 42."
		}
		if slices.ContainsFunc(result.Parts, isDisplay) != enabled || slices.ContainsFunc(reported, isDisplay) != enabled {
			t.Fatalf("plugin enabled=%t: persisted parts %+v, live parts %+v", enabled, result.Parts, reported)
		}
	}
}

func TestDisplaySpecRejectsUnsupportedAndUnboundedUI(t *testing.T) {
	valid, err := validateDisplaySpec(metricSpec)
	if err != nil || valid != metricSpec {
		t.Fatalf("valid spec produced %q: %v", valid, err)
	}
	invalid := map[string]string{
		"unsupported":  strings.Replace(metricSpec, "Metric", "Script", 1),
		"actions":      `{"root":"a","elements":{"a":{"type":"Stack","props":{},"on":{"click":{"action":"execute"}}}}}`,
		"cycle":        `{"root":"a","elements":{"a":{"type":"Stack","props":{},"children":["a"]}}}`,
		"invalidProps": strings.Replace(metricSpec, `"42"`, `42`, 1),
		"oversize":     strings.Repeat(" ", partBytes) + metricSpec,
	}
	for name, spec := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := validateDisplaySpec(spec); err == nil {
				t.Fatal("accepted invalid spec")
			}
		})
	}
}
