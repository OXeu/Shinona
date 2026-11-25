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

	"github.com/guanshan/pi-go/packages/ai"
)

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
	base    *url.URL
	key     string
	client  *http.Client
	mu      sync.Mutex
	models  []ModelInfo
	expires time.Time
	flight  *catalogFetch
}

type catalogFetch struct {
	done   chan struct{}
	models []ModelInfo
	err    error
}

func NewEndpoint(base, key string, client *http.Client) (*Endpoint, error) {
	target, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery ||
		target.Fragment != "" || (target.Scheme != "https" && target.Scheme != "http") {
		return nil, errors.New("enter an HTTP(S) OpenAI-compatible base URL without credentials, query, or fragment")
	}
	return &Endpoint{base: target, key: key, client: client}, nil
}

func (e *Endpoint) Catalog(ctx context.Context, force bool) ([]ModelInfo, error) {
	models, flight := e.catalog(force)
	if flight == nil {
		return models, nil
	}
	select {
	case <-ctx.Done():
		return models, ctx.Err()
	case <-flight.done:
		return slices.Clone(flight.models), flight.err
	}
}

// CachedCatalog starts a bounded refresh when the cache expires and returns
// immediately so model discovery does not delay access to composer settings.
func (e *Endpoint) CachedCatalog() ([]ModelInfo, bool) {
	models, flight := e.catalog(false)
	return models, flight != nil
}

func (e *Endpoint) catalog(force bool) ([]ModelInfo, *catalogFetch) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.flight == nil && (force || !time.Now().Before(e.expires)) {
		e.flight = &catalogFetch{done: make(chan struct{})}
		go e.refreshCatalog(e.flight)
	}
	return slices.Clone(e.models), e.flight
}

func (e *Endpoint) refreshCatalog(flight *catalogFetch) {
	models, err := e.fetch(context.Background())
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.expires = time.Now().Add(30 * time.Second)
	} else {
		e.models, e.expires = models, time.Now().Add(5*time.Minute)
	}
	flight.models = e.models
	flight.err = err
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
	resp, err := e.client.Do(req)
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
	if err := decoder.Decode(&catalog); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF || body.N <= 0 || catalog.Data == nil {
		return nil, errors.New("invalid model catalog")
	}
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
	model := ai.Model{ID: id, Name: id, Input: []string{"text", "image"}}
	if strings.EqualFold(e.base.Hostname(), "api.openai.com") {
		if known, ok := ai.Find(ai.GeneratedModels(), "openai", id); ok {
			model = known
		}
	}
	e.mu.Lock()
	if index := slices.IndexFunc(e.models, func(entry ModelInfo) bool { return entry.ID == id }); index >= 0 &&
		e.models[index].ContextWindow > 0 {
		model.ContextWindow = int(e.models[index].ContextWindow)
	}
	e.mu.Unlock()
	model.Provider, model.API, model.BaseURL = "openai", "openai-completions", e.base.String()
	model.EnvKey = ""
	model.MaxOutput = 0
	model.Compat.SupportsStore = new(false)
	if !strings.EqualFold(e.base.Hostname(), "api.openai.com") {
		model.Compat.SupportsDeveloperRole = new(false)
	}
	return model
}
