package pigo

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/guanshan/pi-go/packages/agent"
)

type renderedUI struct {
	source  string
	summary string
}

func arrowRenderTool() agent.AgentTool {
	return tool{
		name: "render_ui",
		description: "Display a custom interactive ArrowJS view in the conversation. " +
			"Provide source as JavaScript (main.js) with a default export from the html tagged template, and summary as a concise text equivalent. " +
			"Import html, reactive, and component from @arrow-js/core. You can compose HTML and inline SVG and use reactive state and events. " +
			"For example: import { html, reactive } from '@arrow-js/core'; const state = reactive({ count: 0 }); " +
			"export default html`<button type=\"button\" @click=\"${() => state.count++}\">Count ${() => state.count}</button>`. " +
			"Provide optional css for custom styling. Use var(--primary), var(--surface), var(--border), and var(--muted) to match the app. " +
			"Use self-contained data. The view cannot access network, storage, the parent page, or agent tools.",
		fields:   []string{"source", "summary"},
		optional: []string{"css"},
		execute: func(_ context.Context, args map[string]string) (agent.AgentToolResult, error) {
			summary := strings.TrimSpace(args["summary"])
			if summary == "" || utf8.RuneCountInString(summary) > 320 {
				return toolFailure("Provide a plain-text summary of 1 to 320 characters."), nil
			}
			source, css := strings.TrimSpace(args["source"]), args["css"]
			if source == "" || len(source) > 16<<10 || len(css) > 8<<10 {
				return toolFailure("Provide JavaScript source of up to 16 KiB and optional CSS of up to 8 KiB."), nil
			}
			files := map[string]string{"main.js": source}
			if css != "" {
				files["main.css"] = css
			}
			payload, err := json.Marshal(files)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			result := textResult("Added the interactive view to the conversation.")
			result.Details = renderedUI{source: string(payload), summary: summary}
			return result, nil
		},
	}
}
