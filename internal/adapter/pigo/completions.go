package pigo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"

	"github.com/OXeu/shinona/internal/run"
)

const (
	modelResponseIdle   = 3 * time.Minute
	modelIdleDiagnostic = "model_idle"
)

func (e *Endpoint) stream(ctx context.Context, model *ai.Model, input ai.TranscriptContext,
	options *ai.SimpleStreamOptions, report func(run.ContextUsage),
) *ai.AssistantMessageEventStream {
	request, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(e.completionIdle, func() { cancel(run.ErrModelIdle) })
	resetIdle := func() {
		if timer.Stop() {
			timer.Reset(e.completionIdle)
		}
	}

	opts := *options
	opts.APIKey = e.key
	opts.Identity = &ai.ClientIdentity{Attribution: ai.NoAttribution}
	client := *e.client
	client.Timeout = 0
	opts.HTTPClient = completionClient{&client}
	opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
		if body, ok := payload.(map[string]any); ok {
			delete(body, "max_tokens")
			delete(body, "max_completion_tokens")
		}
		return payload, nil
	}
	var lastTokens int
	opts.OnProviderStreamEvent = func(data any, _ *ai.Model) error {
		chunk, _ := data.(ai.OrderedObject)
		tokens := completionUsage(chunk)
		if tokens > 0 && tokens != lastTokens {
			resetIdle()
			lastTokens = tokens
			if report != nil && request.Err() == nil {
				report(run.ContextUsage{Tokens: int32(tokens), Window: int32(model.ContextWindow)})
			}
		}
		return nil
	}

	source := providers.StreamSimpleOpenAICompletions(request, model, input, &opts)
	output := ai.NewAssistantMessageEventStream()
	go func() {
		defer cancel(nil)
		defer timer.Stop()
		defer output.End()
		for event := range source.Events() {
			switch event.Type {
			case ai.EventToolCallStart:
				resetIdle()
			case ai.EventTextDelta, ai.EventThinkingDelta, ai.EventToolCallDelta:
				if event.Delta != "" {
					resetIdle()
				}
			}
			if event.Type == ai.EventError && event.Error != nil {
				message := event.Error.Clone()
				if e.key != "" {
					message.ErrorMessage = strings.ReplaceAll(message.ErrorMessage, e.key, "[redacted]")
				}
				if errors.Is(context.Cause(request), run.ErrModelIdle) {
					event.Reason = ai.StopError
					message.StopReason = ai.StopError
					message.Diagnostics = append(message.Diagnostics, ai.Diagnostic{Type: modelIdleDiagnostic})
				}
				event.Error = message
			}
			output.Push(event)
		}
	}()
	return output
}

func completionUsage(chunk ai.OrderedObject) int {
	value, _ := chunk.Get("usage")
	usage, _ := value.(ai.OrderedObject)
	number := func(field string) int {
		value, _ := usage.Get(field)
		switch count := value.(type) {
		case float64:
			return int(count)
		case json.Number:
			parsed, _ := count.Int64()
			return int(parsed)
		default:
			return 0
		}
	}
	if total := number("total_tokens"); total > 0 {
		return total
	}
	return number("prompt_tokens") + number("completion_tokens")
}

type completionClient struct{ *http.Client }

// Some compatible endpoints return a JSON completion even when streaming is requested.
func (c completionClient) Do(req *http.Request) (*http.Response, error) {
	response, err := c.Client.Do(req)
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 ||
		strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "event-stream") {
		return response, err
	}
	defer response.Body.Close()
	var completion map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&completion); err != nil {
		return nil, err
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(completion["choices"], &choices); err != nil {
		return nil, fmt.Errorf("decode compatible model response: %w", err)
	}
	for _, choice := range choices {
		choice["delta"] = choice["message"]
		delete(choice, "message")
	}
	completion["choices"], err = json.Marshal(choices)
	if err != nil {
		return nil, err
	}
	frame, err := json.Marshal(completion)
	if err != nil {
		return nil, err
	}
	var stream bytes.Buffer
	stream.WriteString("data: ")
	stream.Write(frame)
	stream.WriteString("\n\ndata: [DONE]\n\n")
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	response.Header.Set("Content-Type", "text/event-stream")
	response.Header.Del("Content-Length")
	response.ContentLength = int64(stream.Len())
	response.Body = io.NopCloser(&stream)
	return response, nil
}
