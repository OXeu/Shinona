package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/OXeu/shinona/internal/adapter/httpclient"
	"github.com/OXeu/shinona/internal/adapter/pigo"
	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/keylock"
	"github.com/OXeu/shinona/internal/responsecache"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
)

const modelEndpointCacheSize = 128

type modelRuntime struct {
	repo   settings.ModelRepository
	client *http.Client
	policy httpclient.ModelPolicy
	cache  *responsecache.Cache[string, modelEndpoint]
	owners keylock.Locks[string]
	mcp    *mcpRuntime
}

type modelEndpoint struct {
	base     string
	key      string
	endpoint *pigo.Endpoint
}

func newModelRuntime(repo settings.ModelRepository, policy httpclient.ModelPolicy, mcpSettings *mcpRuntime) *modelRuntime {
	return &modelRuntime{
		repo: repo, client: policy.Client(), policy: policy, mcp: mcpSettings,
		cache: responsecache.New[string, modelEndpoint](modelEndpointCacheSize, modelEndpointCacheSize),
	}
}

func (m *modelRuntime) settings(ctx context.Context, owner string) (settings.Values, error) {
	if owner == "" {
		return nil, errors.New("model settings require an authenticated account")
	}
	return m.repo.ModelSettings(ctx, owner)
}

func (m *modelRuntime) load(ctx context.Context, owner string) (settings.Values, *pigo.Endpoint, error) {
	release, err := m.owners.Acquire(ctx, owner, false)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	values, err := m.settings(ctx, owner)
	if err != nil {
		return nil, nil, err
	}
	base, key := values[settings.ModelBaseURL], values[settings.ModelAPIKey]
	if base == "" {
		return values, nil, nil
	}
	if cached, ok := m.cache.Get(owner); ok && cached.base == base && cached.key == key {
		return values, cached.endpoint, nil
	}
	endpoint, err := m.endpoint(base, key)
	if err != nil {
		return nil, nil, err
	}
	m.cache.Put(owner, modelEndpoint{base: base, key: key, endpoint: endpoint}, 1)
	return values, endpoint, nil
}

func (m *modelRuntime) endpoint(base, key string) (*pigo.Endpoint, error) {
	endpoint, err := pigo.NewEndpoint(base, key, m.client)
	if err != nil {
		return nil, settings.Invalid("INVALID_MODEL_BASE_URL", err.Error())
	}
	if err := m.policy.CheckEndpoint(base); err != nil {
		return nil, settings.Invalid("PRIVATE_ENDPOINT", err.Error())
	}
	return endpoint, nil
}

func (m *modelRuntime) ModelFields(ctx context.Context) ([]settings.Field, error) {
	values, err := m.settings(ctx, auth.CurrentUser(ctx).ID)
	if err != nil {
		return nil, err
	}
	return settings.ModelFields(values), nil
}

func (m *modelRuntime) UpdateModelSettings(ctx context.Context, changes []settings.Change) ([]settings.Field, error) {
	owner := auth.CurrentUser(ctx).ID
	release, err := m.owners.Acquire(ctx, owner, false)
	if err != nil {
		return nil, err
	}
	defer release()
	previous, err := m.settings(ctx, owner)
	if err != nil {
		return nil, err
	}
	next, err := settings.ApplyModel(previous, changes)
	if err != nil {
		return nil, err
	}
	base := next[settings.ModelBaseURL]
	if !sameModelBase(base, previous[settings.ModelBaseURL]) && !slices.ContainsFunc(changes, func(change settings.Change) bool {
		return change.Key == settings.ModelAPIKey
	}) {
		delete(next, settings.ModelAPIKey)
	}
	if base != "" {
		if _, err := m.endpoint(base, next[settings.ModelAPIKey]); err != nil {
			return nil, err
		}
	}
	if err := m.repo.SaveModelSettings(ctx, owner, next); err != nil {
		return nil, err
	}
	m.cache.Delete(owner)
	return settings.ModelFields(next), nil
}

func (m *modelRuntime) resolveAgent(ctx context.Context, owner string) (run.Agent, error) {
	values, endpoint, err := m.load(ctx, owner)
	if err != nil {
		return nil, err
	}
	id := values[settings.ModelID]
	if endpoint == nil || id == "" || values[settings.ModelAPIKey] == "" {
		return nil, run.ErrModelConfig
	}
	tools, err := m.mcp.tools(ctx, owner)
	if err != nil {
		return nil, err
	}
	return pigo.New(endpoint, id, tools), nil
}

func (m *modelRuntime) ComposerOptions(ctx context.Context) (rest.ComposerOptions, error) {
	owner := auth.CurrentUser(ctx).ID
	values, endpoint, err := m.load(ctx, owner)
	if err != nil {
		return rest.ComposerOptions{}, err
	}
	tools, err := m.mcp.tools(ctx, owner)
	if err != nil {
		return rest.ComposerOptions{}, err
	}
	options := rest.ComposerOptions{
		Models: []rest.ModelInfo{}, MCPServers: tools.Names(ctx, owner),
	}
	if endpoint == nil {
		return options, nil
	}
	models, loading := endpoint.CachedCatalog()
	options.ModelsLoading = loading
	options.Models = modelInfos(models)
	id := values[settings.ModelID]
	options.DefaultModel = id
	if id != "" {
		window := int32(endpoint.Model(id).ContextWindow)
		for i, model := range options.Models {
			if model.ID == id {
				options.Models[i].ContextWindow = window
				return options, nil
			}
		}
		options.Models = append([]rest.ModelInfo{{ID: id, ContextWindow: window}}, options.Models...)
	}
	return options, nil
}

func (m *modelRuntime) FetchModels(ctx context.Context, base, key *string) ([]rest.ModelInfo, error) {
	values, endpoint, err := m.load(ctx, auth.CurrentUser(ctx).ID)
	if err != nil {
		return nil, err
	}
	savedBase, savedKey := values[settings.ModelBaseURL], values[settings.ModelAPIKey]
	target := savedBase
	if base != nil {
		target = strings.TrimSpace(*base)
	}
	token := ""
	if key != nil {
		token = strings.TrimSpace(*key)
	} else if sameModelBase(target, savedBase) {
		token = savedKey
	}
	if endpoint == nil || !sameModelBase(target, savedBase) || token != savedKey {
		endpoint, err = m.endpoint(target, token)
		if err != nil {
			return nil, err
		}
	}
	models, err := endpoint.Catalog(ctx, true)
	if err != nil {
		return nil, settings.Invalid("MODEL_FETCH_FAILED", "could not fetch models from the configured endpoint")
	}
	return modelInfos(models), nil
}

func sameModelBase(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

func modelInfos(models []pigo.ModelInfo) []rest.ModelInfo {
	result := make([]rest.ModelInfo, len(models))
	for i, model := range models {
		result[i] = rest.ModelInfo{ID: model.ID, ContextWindow: model.ContextWindow}
	}
	return result
}
