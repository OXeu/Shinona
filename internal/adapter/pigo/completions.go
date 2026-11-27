package pigo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"

	"github.com/OXeu/shinona/internal/run"
)

const (
	modelResponseIdle    = 3 * time.Minute
	modelCompletionDrain = 2 * time.Second
	modelIdleDiagnostic  = "model_idle"
	maxModelStreamBytes  = 32 << 20
	maxModelErrorBytes   = 64 << 10
)

func (e *Endpoint) stream(ctx context.Context, model *ai.Model, input ai.TranscriptContext,
	options *ai.SimpleStreamOptions, report func(run.ContextUsage),
) *ai.AssistantMessageEventStream {
	request, cancel := context.WithCancelCause(ctx)
	httpContext, cancelHTTP := context.WithCancel(request)
	var idleMu sync.Mutex
	var draining bool
	timer := time.AfterFunc(e.completionIdle, func() {
		idleMu.Lock()
		finished := draining
		idleMu.Unlock()
		if finished {
			// Pi treats a canceled body read as the end of the SSE stream.
			// Keep its own context alive so it can finish the declared response.
			cancelHTTP()
			return
		}
		cancel(run.ErrModelIdle)
	})
	resetIdle := func() {
		idleMu.Lock()
		defer idleMu.Unlock()
		if !draining && timer.Stop() {
			timer.Reset(e.completionIdle)
		}
	}
	startDrain := func() {
		idleMu.Lock()
		defer idleMu.Unlock()
		if !draining && timer.Stop() {
			draining = true
			timer.Reset(min(modelCompletionDrain, e.completionIdle))
		}
	}
	stopIdle := func() {
		idleMu.Lock()
		defer idleMu.Unlock()
		timer.Stop()
	}

	opts := *options
	opts.APIKey = e.key
	opts.Identity = &ai.ClientIdentity{Attribution: ai.NoAttribution}
	client := *e.client
	client.Timeout = 0
	transport := &completionClient{Client: &client, httpContext: httpContext, streamLimit: e.streamLimit}
	opts.HTTPClient = transport
	var completed *ai.AssistantMessage
	onResponse := opts.OnResponse
	opts.OnResponse = func(response ai.ProviderResponse, model *ai.Model) error {
		if onResponse != nil {
			if err := onResponse(response, model); err != nil {
				return err
			}
		}
		if transport.completedBody == nil {
			return nil
		}
		body := jsonCompletionReader{Reader: transport.completedBody, progress: resetIdle}
		reply, err := decodeJSONCompletion(body, model)
		if err != nil {
			return err
		}
		if err := request.Err(); err != nil {
			return err
		}
		completed = reply
		// The SDK owns the request and closes the body. Stop its SSE reader
		// here and emit a completed Pi message on our stream instead.
		return errCompletedJSON
	}
	opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
		if body, ok := payload.(map[string]any); ok {
			delete(body, "max_tokens")
			delete(body, "max_completion_tokens")
		}
		return payload, nil
	}
	var lastTokens int
	reportUsage := func(tokens int) {
		if tokens <= 0 || tokens == lastTokens {
			return
		}
		lastTokens = tokens
		if report != nil && request.Err() == nil {
			report(run.ContextUsage{Tokens: int32(tokens), Window: int32(model.ContextWindow)})
		}
	}
	var generatedHighWater int
	opts.OnProviderStreamEvent = func(data any, _ *ai.Model) error {
		chunk, _ := data.(ai.OrderedObject)
		var choice ai.OrderedObject
		value, _ := chunk.Get("choices")
		if choices, _ := value.([]any); len(choices) > 0 {
			choice, _ = choices[0].(ai.OrderedObject)
		}
		finish, _ := choice.Get("finish_reason")
		if reason, _ := finish.(string); reason != "" {
			startDrain()
		}
		total, generated := completionUsage(chunk, choice)
		if generated > generatedHighWater {
			generatedHighWater = generated
			resetIdle()
		}
		reportUsage(total)
		return nil
	}

	source := providers.StreamSimpleOpenAICompletions(request, model, input, &opts)
	output := ai.NewAssistantMessageEventStream()
	go func() {
		defer cancel(nil)
		defer cancelHTTP()
		defer stopIdle()
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
				if completed != nil && request.Err() == nil {
					completed.Timestamp = event.Error.Timestamp
					completed.DurationMs = event.Error.DurationMs
					reportUsage(contextTokens(completed.Usage))
					output.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: &ai.AssistantMessage{
						Content: ai.ContentList{}, Api: completed.Api, Provider: completed.Provider,
						Model: completed.Model, StopReason: ai.StopPending, Timestamp: completed.Timestamp,
					}})
					event = completionEvent(completed)
					if event.Type == ai.EventDone {
						output.Push(event)
						return
					}
				}
				message := event.Error.Clone()
				if completed != nil && request.Err() != nil {
					event.Reason = ai.StopAborted
					message.StopReason = ai.StopAborted
					message.ErrorMessage = request.Err().Error()
				}
				if e.key != "" {
					message.ErrorMessage = strings.ReplaceAll(message.ErrorMessage, e.key, "[redacted]")
					message.RawStopReason = strings.ReplaceAll(message.RawStopReason, e.key, "[redacted]")
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

func completionUsage(chunk, choice ai.OrderedObject) (int, int) {
	value, _ := chunk.Get("usage")
	usage, ok := value.(ai.OrderedObject)
	if !ok {
		// Pi also accepts providers that report usage on the first choice.
		value, _ := choice.Get("usage")
		usage, _ = value.(ai.OrderedObject)
	}
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
	generated := number("completion_tokens")
	total := number("total_tokens")
	if total <= 0 {
		total = number("prompt_tokens") + generated
	}
	return total, generated
}

var (
	errCompletedJSON         = errors.New("handled completed JSON response")
	errModelResponseTooLarge = errors.New("model completion exceeds the response size limit")
)

type completionClient struct {
	*http.Client
	httpContext   context.Context
	streamLimit   int64
	completedBody io.Reader
}

// Some compatible endpoints return a JSON completion even when streaming is requested.
func (c *completionClient) Do(req *http.Request) (*http.Response, error) {
	response, err := c.Client.Do(req.WithContext(c.httpContext))
	c.completedBody = nil
	if err != nil {
		return response, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body = limitCompletionBody(response.Body, maxModelErrorBytes)
		return response, nil
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		// Bound the SDK's read-ahead before it has found an event separator.
		response.Body = limitCompletionBody(response.Body, c.streamLimit)
	} else {
		c.completedBody = response.Body
	}
	return response, nil
}

type limitedCompletionBody struct{ io.ReadCloser }

func limitCompletionBody(body io.ReadCloser, limit int64) io.ReadCloser {
	return limitedCompletionBody{http.MaxBytesReader(nil, body, limit)}
}

func (body limitedCompletionBody) Read(data []byte) (int, error) {
	n, err := body.ReadCloser.Read(data)
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		return n, errModelResponseTooLarge
	}
	return n, err
}
