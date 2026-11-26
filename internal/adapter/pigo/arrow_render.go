package pigo

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

//go:embed arrow-view.json
var arrowViewJSON []byte

type arrowViewContract struct {
	Format            string   `json:"format"`
	ScriptFile        string   `json:"scriptFile"`
	StyleFile         string   `json:"styleFile"`
	ScriptBytes       int      `json:"scriptBytes"`
	StyleBytes        int      `json:"styleBytes"`
	SummaryCharacters int      `json:"summaryCharacters"`
	ModuleName        string   `json:"moduleName"`
	Example           string   `json:"example"`
	ThemeTokens       []string `json:"themeTokens"`
}

var arrowView = func() arrowViewContract {
	var contract arrowViewContract
	if err := json.Unmarshal(arrowViewJSON, &contract); err != nil {
		panic(fmt.Errorf("read embedded ArrowJS view contract: %w", err))
	}
	return contract
}()

type renderedUI struct {
	source  string
	summary string
}

func arrowRenderTool() agent.AgentTool {
	return agent.AgentTool{
		Name: "render_ui", Label: "render_ui",
		Description: "Display a custom interactive ArrowJS view in the conversation. " +
			"Provide source as JavaScript (" + arrowView.ScriptFile + ") with a default export from the html tagged template, " +
			"and summary as a concise text equivalent. " +
			"Import html, reactive, and component from " + arrowView.ModuleName + ". You can compose HTML and inline SVG and use reactive state and events. " +
			"For example: import { html, reactive } from '" + arrowView.ModuleName + "'; " + arrowView.Example + " " +
			"Provide optional css for custom styling. Use var() with the available theme variables " + strings.Join(arrowView.ThemeTokens, ", ") + ". " +
			"Use self-contained data. The view cannot access network, storage, the parent page, or agent tools.",
		Parameters: toolParameters(ai.Prop("source", ai.String()), ai.Prop("summary", ai.String()), ai.Opt("css", ai.String())),
		Execute: func(_ context.Context, _ string, args map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			summary := strings.TrimSpace(args["summary"].(string))
			if summary == "" || utf8.RuneCountInString(summary) > arrowView.SummaryCharacters {
				return toolFailure(fmt.Sprintf("Provide a plain-text summary of 1 to %d characters.", arrowView.SummaryCharacters)), nil
			}
			source := strings.TrimSpace(args["source"].(string))
			css, _ := args["css"].(string)
			if source == "" || len(source) > arrowView.ScriptBytes || len(css) > arrowView.StyleBytes {
				return toolFailure(fmt.Sprintf("Provide JavaScript source of up to %d bytes and optional CSS of up to %d bytes.",
					arrowView.ScriptBytes, arrowView.StyleBytes)), nil
			}
			files := map[string]string{arrowView.ScriptFile: source}
			if css != "" {
				files[arrowView.StyleFile] = css
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
