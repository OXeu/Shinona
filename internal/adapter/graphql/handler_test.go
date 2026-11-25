package graphql_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/graphql"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

func TestConversationLifecycleAndOwnership(t *testing.T) {
	ctx, stop := context.WithTimeout(t.Context(), 6*time.Second)
	defer stop()
	repo, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})

	logger := slog.New(slog.DiscardHandler)
	target := func(string) run.Target {
		return run.Target{Name: "demo", Provider: demo.Provider{}, ResolveAgent: func(context.Context, string) (run.Agent, error) { return progressAgent{}, nil }}
	}
	controls := &run.Controls{}
	service := run.NewService(repo, target, uuid.NewString, controls)
	handler, err := graphql.NewHandler(service, application{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	alice := auth.WithUser(ctx, auth.User{ID: "alice"})
	bob := auth.WithUser(ctx, auth.User{ID: "bob"})
	created := query(t, handler, alice, `mutation { createSession { id defaultTitle } }`, nil)
	id := created.Data.CreateSession.ID
	if len(created.Errors) != 0 || id == "" || !created.Data.CreateSession.DefaultTitle {
		t.Fatalf("create session: %+v", created)
	}

	save := query(t, handler, alice, `mutation { setSandboxPolicy(policy:SHARED) { selected } }`, nil)
	if len(save.Errors) != 0 || save.Data.SetSandboxPolicy.Selected != "SHARED" {
		t.Fatalf("save policy: %+v", save)
	}
	settings := query(t, handler, bob, `{ sandboxSettings { selected } }`, nil)
	if len(settings.Errors) != 0 || settings.Data.SandboxSettings.Selected != "FRESH" {
		t.Fatalf("other account settings: %+v", settings)
	}

	const submit = `mutation AddTurn($id: ID!, $prompt: String!) { submitRun(sessionId:$id, prompt:$prompt, payload:{jsonRender:true}) { id status policy } }`
	variables := map[string]string{"id": id, "prompt": "first prompt"}
	unknown := query(t, handler, bob, submit, variables)
	if len(unknown.Errors) != 1 || unknown.Errors[0].Message != run.ErrSession.Error() ||
		unknown.Errors[0].Extensions.Code != "SESSION_NOT_FOUND" {
		t.Fatalf("cross-account submit: %+v", unknown)
	}
	first := query(t, handler, alice, submit, variables)
	if len(first.Errors) != 0 || first.Data.SubmitRun.Status != run.Queued || first.Data.SubmitRun.Policy != "SHARED" {
		t.Fatalf("submit first turn: %+v", first)
	}
	duplicate := query(t, handler, alice, submit, variables)
	if len(duplicate.Errors) != 1 || duplicate.Errors[0].Message != run.ErrActiveRun.Error() ||
		duplicate.Errors[0].Extensions.Code != "ACTIVE_TURN" {
		t.Fatalf("concurrent turn: %+v", duplicate)
	}

	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.NewWorker(repo, target, logger, controls).Serve(workerCtx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	firstTranscript := waitForTurn(t, handler, alice, id, 1)
	if firstTranscript.LastOptions == nil || !firstTranscript.LastOptions.JSONRender {
		t.Fatal("json-render selection was not persisted and returned")
	}
	if !strings.Contains(firstTranscript.Turns[0].Output, "0 earlier turns") {
		t.Fatalf("first turn used history: %+v", firstTranscript)
	}
	if turn := firstTranscript.Turns[0]; turn.StartedAt == nil || len(turn.Parts) != 1 || turn.Parts[0].Text != "Review the request." {
		t.Fatalf("turn progress was not stored: %+v", turn)
	}
	save = query(t, handler, alice, `mutation { setSandboxPolicy(policy:FRESH) { selected } }`, nil)
	if len(save.Errors) != 0 {
		t.Fatalf("change policy: %+v", save)
	}
	variables["prompt"] = "follow-up prompt"
	second := query(t, handler, alice, submit, variables)
	if len(second.Errors) != 0 || second.Data.SubmitRun.Policy != "FRESH" {
		t.Fatalf("submit second turn: %+v", second)
	}
	transcript := waitForTurn(t, handler, alice, id, 2)
	if transcript.TurnCount != 2 || transcript.DefaultTitle || transcript.Title != "first prompt" ||
		!strings.Contains(transcript.Turns[1].Output, "1 earlier turns") ||
		transcript.Turns[0].Policy != "SHARED" || transcript.Turns[1].Policy != "FRESH" {
		t.Fatalf("unexpected transcript: %+v", transcript)
	}
	result := query(t, handler, bob, `query Other($id: ID!) { session(id:$id) { id } sessions { id } }`, variables)
	if len(result.Errors) != 0 || result.Data.Session != nil || len(result.Data.Sessions) != 0 {
		t.Fatalf("cross-account read: %+v", result)
	}

	draft := query(t, handler, alice, `mutation { createSession { id defaultTitle } }`, nil)
	if len(draft.Errors) != 0 || !draft.Data.CreateSession.DefaultTitle {
		t.Fatalf("create draft: %+v", draft)
	}
	named := query(t, handler, alice, `mutation Name($id: ID!) {
		updateSession(id: $id, title: "New session") { title defaultTitle }
	}`, map[string]string{"id": draft.Data.CreateSession.ID})
	if len(named.Errors) != 0 || named.Data.UpdateSession.DefaultTitle || named.Data.UpdateSession.Title != "New session" {
		t.Fatalf("explicit title must remain user content: %+v", named)
	}
}

func TestComposerDefaultsAreAccountScopedAndValidated(t *testing.T) {
	ctx := t.Context()
	repo, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	service := run.NewService(repo, nil, nil, &run.Controls{})
	handler, err := graphql.NewHandler(service, application{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	alice := auth.WithUser(ctx, auth.User{ID: "alice"})
	bob := auth.WithUser(ctx, auth.User{ID: "bob"})
	const read = `{ composerOptions { defaults { model permission thinking } } }`
	const write = `mutation SetDefaults($model: String!, $permission: String!, $thinking: String!) {
		setComposerDefaults(input:{model:$model,permission:$permission,thinking:$thinking}) { model permission thinking }
	}`
	first := query(t, handler, alice, read, nil)
	if len(first.Errors) != 0 || first.Data.ComposerOptions.Defaults != nil {
		t.Fatalf("expected no saved defaults: %+v", first)
	}
	input := map[string]string{"model": "fixture-model", "permission": "READ_ONLY", "thinking": "high"}
	want := run.ComposerDefaults{Model: "fixture-model", Permission: "READ_ONLY", Thinking: "high"}
	saved := query(t, handler, alice, write, input)
	if len(saved.Errors) != 0 || saved.Data.SetComposerDefaults != want {
		t.Fatalf("save composer defaults: %+v", saved)
	}
	input["thinking"] = "unknown"
	invalid := query(t, handler, alice, write, input)
	if len(invalid.Errors) != 1 || invalid.Errors[0].Extensions.Code != "INVALID_COMPOSER_OPTIONS" {
		t.Fatalf("invalid defaults should be rejected: %+v", invalid)
	}
	readBack := query(t, handler, alice, read, nil)
	if len(readBack.Errors) != 0 || readBack.Data.ComposerOptions.Defaults == nil || *readBack.Data.ComposerOptions.Defaults != want {
		t.Fatalf("read saved defaults: %+v", readBack)
	}
	other := query(t, handler, bob, read, nil)
	if len(other.Errors) != 0 || other.Data.ComposerOptions.Defaults != nil {
		t.Fatalf("defaults leaked across accounts: %+v", other)
	}
	input["thinking"] = "medium"
	want.Thinking = "medium"
	updated := query(t, handler, alice, write, input)
	if len(updated.Errors) != 0 || updated.Data.SetComposerDefaults != want {
		t.Fatalf("replace composer defaults: %+v", updated)
	}
}

func TestProjectSessionsAndOwnership(t *testing.T) {
	repo, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	service := run.NewService(repo, nil, uuid.NewString, &run.Controls{})
	handler, err := graphql.NewHandler(service, application{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	alice := auth.WithUser(t.Context(), auth.User{ID: "alice"})
	bob := auth.WithUser(t.Context(), auth.User{ID: "bob"})

	const add = `mutation AddProject($name: String!) { createProject(name: $name) { id name } }`
	added := query(t, handler, alice, add, map[string]string{"name": "  Website  refresh "})
	project := added.Data.CreateProject
	if len(added.Errors) != 0 || project.ID == "" || project.Name != "Website refresh" {
		t.Fatalf("create project: %+v", added)
	}
	duplicate := query(t, handler, alice, add, map[string]string{"name": project.Name})
	if len(duplicate.Errors) != 1 || duplicate.Errors[0].Extensions.Code != "PROJECT_NAME_EXISTS" {
		t.Fatalf("duplicate project: %+v", duplicate)
	}
	invalid := query(t, handler, alice, add, map[string]string{"name": "  "})
	if len(invalid.Errors) != 1 || invalid.Errors[0].Extensions.Code != "INVALID_PROJECT_NAME" {
		t.Fatalf("invalid project: %+v", invalid)
	}
	list := query(t, handler, bob, `{ projects { id name } }`, nil)
	if len(list.Errors) != 0 || len(list.Data.Projects) != 0 {
		t.Fatalf("cross-account project list: %+v", list)
	}

	const create = `mutation NewSession($id: ID) { createSession(projectId: $id) { id projectId } }`
	projectArg := map[string]string{"id": project.ID}
	denied := query(t, handler, bob, create, projectArg)
	if len(denied.Errors) != 1 || denied.Errors[0].Extensions.Code != "PROJECT_NOT_FOUND" {
		t.Fatalf("cross-account session assignment: %+v", denied)
	}
	defaultSession := query(t, handler, alice, create, nil)
	if len(defaultSession.Errors) != 0 || defaultSession.Data.CreateSession.ID == "" || defaultSession.Data.CreateSession.ProjectID != nil {
		t.Fatalf("default session: %+v", defaultSession)
	}
	assigned := query(t, handler, alice, create, projectArg)
	if len(assigned.Errors) != 0 || assigned.Data.CreateSession.ProjectID == nil || *assigned.Data.CreateSession.ProjectID != project.ID {
		t.Fatalf("assigned session: %+v", assigned)
	}

	const sessions = `query Sessions($id: ID) { projects { id name } sessions(projectId: $id) { id projectId } }`
	all := query(t, handler, alice, sessions, nil)
	if len(all.Errors) != 0 || len(all.Data.Sessions) != 2 || len(all.Data.Projects) != 1 || all.Data.Projects[0] != project {
		t.Fatalf("all projects: %+v", all)
	}
	for _, test := range []struct {
		projectID string
		sessionID string
	}{
		{"", defaultSession.Data.CreateSession.ID},
		{project.ID, assigned.Data.CreateSession.ID},
	} {
		filtered := query(t, handler, alice, sessions, map[string]string{"id": test.projectID})
		if len(filtered.Errors) != 0 || len(filtered.Data.Sessions) != 1 || filtered.Data.Sessions[0].ID != test.sessionID {
			t.Fatalf("filter %q: %+v", test.projectID, filtered)
		}
	}
	hidden := query(t, handler, bob, sessions, projectArg)
	if len(hidden.Errors) != 0 || len(hidden.Data.Sessions) != 0 {
		t.Fatalf("cross-account session filter: %+v", hidden)
	}

	const rename = `mutation Rename($id: ID!, $name: String!) { renameProject(id: $id, name: $name) { id name } }`
	name := map[string]string{"id": project.ID, "name": " Documentation  refresh "}
	if other := query(t, handler, bob, rename, name); len(other.Errors) != 1 || other.Errors[0].Extensions.Code != "PROJECT_NOT_FOUND" {
		t.Fatalf("cross-account rename: %+v", other)
	}
	renamed := query(t, handler, alice, rename, name)
	if len(renamed.Errors) != 0 || renamed.Data.RenameProject != (projectResult{ID: project.ID, Name: "Documentation refresh"}) {
		t.Fatalf("rename project: %+v", renamed)
	}
	const remove = `mutation Remove($id: ID!) { removeProject(id: $id) }`
	if other := query(t, handler, bob, remove, projectArg); len(other.Errors) != 1 || other.Errors[0].Extensions.Code != "PROJECT_NOT_FOUND" {
		t.Fatalf("cross-account removal: %+v", other)
	}
	if removed := query(t, handler, alice, remove, projectArg); len(removed.Errors) != 0 || !removed.Data.RemoveProject {
		t.Fatalf("remove project: %+v", removed)
	}
	after := query(t, handler, alice, sessions, map[string]string{"id": ""})
	if len(after.Errors) != 0 || len(after.Data.Projects) != 0 || len(after.Data.Sessions) != 2 {
		t.Fatalf("session moved to the default project: %+v", after)
	}
}

type progressAgent struct{}

func (progressAgent) Run(ctx context.Context, input run.Input, ws sandbox.Workspace) (run.Result, error) {
	parts := []run.TurnPart{{Kind: "thinking", Text: "Review the request."}}
	input.ReportParts(parts)
	result, err := (demo.Agent{}).Run(ctx, input, ws)
	result.Parts = parts
	return result, err
}

func waitForTurn(t *testing.T, handler http.Handler, ctx context.Context, id string, count int) sessionResult {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		response := query(t, handler, ctx, `query Session($id: ID!) {
			session(id:$id) { title defaultTitle turnCount lastOptions { jsonRender } turns { status output policy startedAt parts { kind text } } }
		}`, map[string]string{"id": id})
		if len(response.Errors) != 0 {
			t.Fatalf("read transcript: %+v", response)
		}
		item := response.Data.Session
		if item != nil && len(item.Turns) == count && item.Turns[count-1].Status == run.Succeeded {
			return *item
		}
		select {
		case <-ctx.Done():
			t.Fatalf("turn did not complete: %+v", item)
		case <-ticker.C:
		}
	}
}

type turnResult struct {
	ID        string
	Status    run.Status
	Output    string
	Policy    string
	StartedAt *string
	Parts     []run.TurnPart
}

type sessionResult struct {
	ID           string
	ProjectID    *string
	DefaultTitle bool
	Title        string
	TurnCount    int
	LastOptions  *struct{ JSONRender bool }
	Turns        []turnResult
}

type projectResult struct {
	ID   string
	Name string
}

type graphResponse struct {
	Data struct {
		CreateProject       projectResult
		RenameProject       projectResult
		RemoveProject       bool
		Projects            []projectResult
		CreateSession       sessionResult
		UpdateSession       sessionResult
		Session             *sessionResult
		Sessions            []sessionResult
		SubmitRun           turnResult
		SetSandboxPolicy    struct{ Selected string }
		SandboxSettings     struct{ Selected string }
		SetComposerDefaults run.ComposerDefaults
		ComposerOptions     struct{ Defaults *run.ComposerDefaults }
	}
	Errors []struct {
		Message    string
		Extensions struct{ Code string }
	}
}

func query(t *testing.T, handler http.Handler, ctx context.Context, document string, variables map[string]string) graphResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": document, "variables": variables})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/graphql", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var response graphResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response (%d): %v", recorder.Code, err)
	}
	return response
}

type application struct{ graphql.Application }

func (application) Mode() string             { return "demo" }
func (application) Fields() []settings.Field { return []settings.Field{} }
func (application) Update(context.Context, []settings.Change) ([]settings.Field, error) {
	return []settings.Field{}, nil
}

func (application) ComposerOptions(context.Context) (graphql.ComposerOptions, error) {
	return graphql.ComposerOptions{Models: []graphql.ModelInfo{}, MCPServers: []string{}}, nil
}
func (application) FetchModels(context.Context, *string, *string) ([]graphql.ModelInfo, error) {
	return []graphql.ModelInfo{}, nil
}
