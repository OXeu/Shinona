package pigo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sky-valley/pi/ai"

	"github.com/OXeu/shinona/internal/timing"
)

const completionProvider = "openai"

type ModelInfo struct {
	ID            string
	ContextWindow int32
}

// A malformed optional token count should not break model discovery.
type catalogTokenLimit int32

func (n *catalogTokenLimit) UnmarshalJSON(data []byte) error {
	text := string(data)
	value, err := strconv.ParseInt(strings.Trim(text, `"`), 10, 32)
	if err == nil && value > 0 {
		*n = catalogTokenLimit(value)
	}
	return nil
}

type Endpoint struct {
	base           *url.URL
	catalogOrigin  bool
	key            string
	client         *http.Client
	completionIdle time.Duration
	streamLimit    int64
	mu             sync.Mutex
	models         []ModelInfo
	expires        time.Time
	catalogErr     error
	flight         *catalogFetch
}

type catalogFetch struct {
	done   chan struct{}
	models []ModelInfo
	err    error
	phases *timing.Recorder
}

func NewEndpoint(base, key string, client *http.Client) (*Endpoint, error) {
	target, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery ||
		target.Fragment != "" || (target.Scheme != "https" && target.Scheme != "http") {
		return nil, errors.New("enter an HTTP(S) OpenAI-compatible base URL without credentials, query, or fragment")
	}
	catalogOrigin := slices.ContainsFunc(ai.GetModels(completionProvider), func(model *ai.Model) bool {
		return matchesModelOrigin(target, model.BaseURL)
	})
	return &Endpoint{base: target, catalogOrigin: catalogOrigin, key: key, client: client,
		completionIdle: modelResponseIdle, streamLimit: maxModelStreamBytes}, nil
}

func matchesModelOrigin(endpoint *url.URL, modelBase string) bool {
	if endpoint.Scheme != "https" || endpoint.User != nil {
		return false
	}
	catalog, err := url.Parse(modelBase)
	if err != nil || catalog.Scheme != endpoint.Scheme || catalog.Hostname() == "" || catalog.User != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSuffix(endpoint.Hostname(), "."), strings.TrimSuffix(catalog.Hostname(), ".")) &&
		httpsPort(endpoint) == httpsPort(catalog)
}

func httpsPort(endpoint *url.URL) string {
	if port := endpoint.Port(); port != "" {
		return port
	}
	return "443"
}

func (e *Endpoint) Catalog(ctx context.Context, force bool) ([]ModelInfo, error) {
	models, flight, err := e.catalog(force)
	if flight == nil {
		return models, err
	}
	defer timing.Start(ctx, "model_catalog_wait")()
	select {
	case <-ctx.Done():
		return models, ctx.Err()
	case <-flight.done:
		flight.phases.Publish(ctx)
		return slices.Clone(flight.models), flight.err
	}
}

// CachedCatalog starts a bounded refresh when the cache expires and returns
// immediately so model discovery does not delay access to composer settings.
func (e *Endpoint) CachedCatalog() ([]ModelInfo, bool) {
	models, flight, _ := e.catalog(false)
	return models, flight != nil
}

func (e *Endpoint) catalog(force bool) ([]ModelInfo, *catalogFetch, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.flight == nil && (force || !time.Now().Before(e.expires)) {
		e.flight = &catalogFetch{done: make(chan struct{})}
		go e.refreshCatalog(e.flight)
	}
	if len(e.models) == 0 {
		return nil, e.flight, e.catalogErr
	}
	return slices.Clone(e.models), e.flight, nil
}

func (e *Endpoint) refreshCatalog(flight *catalogFetch) {
	ctx, phases := timing.WithRecorder(context.Background())
	models, err := e.fetch(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.expires = time.Now().Add(30 * time.Second)
	} else {
		e.models, e.expires = models, time.Now().Add(5*time.Minute)
	}
	e.catalogErr = err
	flight.models = e.models
	flight.err = err
	flight.phases = phases
	e.flight = nil
	close(flight.done)
}

func (e *Endpoint) fetch(ctx context.Context) ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.base.JoinPath("models").String(), nil)
	if err != nil {
		return nil, err
	}
	if e.key != "" {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	request := timing.Start(ctx, "model_catalog_http")
	resp, err := e.client.Do(req)
	request()
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("the model endpoint did not return a catalog")
	}
	body := &io.LimitedReader{R: resp.Body, N: (2 << 20) + 1}
	var catalog struct {
		Data []struct {
			ID                 string            `json:"id"`
			ContextLength      catalogTokenLimit `json:"context_length"`
			ContextWindow      catalogTokenLimit `json:"context_window"`
			ContextLengthCamel catalogTokenLimit `json:"contextLength"`
			ContextWindowCamel catalogTokenLimit `json:"contextWindow"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(body)
	decode := timing.Start(ctx, "model_catalog_decode")
	err = decoder.Decode(&catalog)
	if err != nil {
		decode()
		return nil, err
	}
	valid := decoder.Decode(new(any)) == io.EOF && body.N > 0 && catalog.Data != nil
	decode()
	if !valid {
		return nil, errors.New("invalid model catalog")
	}
	defer timing.Start(ctx, "model_catalog_build")()
	result := make([]ModelInfo, 0, min(500, len(catalog.Data)))
	seen := make(map[string]bool, cap(result))
	for _, model := range catalog.Data {
		if model.ID == "" || len(model.ID) > 256 || seen[model.ID] {
			continue
		}
		window := max(model.ContextLength, model.ContextWindow, model.ContextLengthCamel, model.ContextWindowCamel, 0)
		result = append(result, ModelInfo{ID: model.ID, ContextWindow: int32(window)})
		seen[model.ID] = true
		if len(result) == 500 {
			break
		}
	}
	slices.SortFunc(result, func(a, b ModelInfo) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}

func (e *Endpoint) Model(id string) ai.Model {
	return e.model(id, false)
}

func (e *Endpoint) model(id string, reasoning bool) ai.Model {
	model := ai.Model{ID: id, Name: id, Input: []string{"text", "image"}}
	if e.catalogOrigin {
		if known := ai.GetModel(completionProvider, id); known != nil && matchesModelOrigin(e.base, known.BaseURL) {
			model = *known
		}
	}
	e.mu.Lock()
	index, found := slices.BinarySearchFunc(e.models, id, func(entry ModelInfo, id string) int {
		return strings.Compare(entry.ID, id)
	})
	if found && e.models[index].ContextWindow > 0 {
		model.ContextWindow = int(e.models[index].ContextWindow)
	}
	e.mu.Unlock()
	model.Provider, model.Api, model.BaseURL = completionProvider, ai.APIOpenAICompletions, e.base.String()
	model.Reasoning = model.Reasoning || reasoning
	model.Compat = completionCompatibility(model.Compat, !e.catalogOrigin, reasoning)
	return model
}

func completionCompatibility(previous json.RawMessage, custom, reasoning bool) json.RawMessage {
	compat := make(map[string]json.RawMessage)
	if len(previous) > 0 {
		_ = json.Unmarshal(previous, &compat)
	}
	if compat == nil {
		compat = make(map[string]json.RawMessage)
	}
	compat["supportsStore"] = json.RawMessage("false")
	if custom {
		compat["supportsDeveloperRole"] = json.RawMessage("false")
	}
	if reasoning {
		compat["supportsReasoningEffort"] = json.RawMessage("true")
	}
	value, _ := json.Marshal(compat)
	return value
}
