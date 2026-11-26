package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
)

type fixtureApplication struct{ Application }

func (fixtureApplication) ComposerOptions(context.Context) (ComposerOptions, error) {
	return ComposerOptions{Models: []ModelInfo{}, MCPServers: []string{}}, nil
}

func restFixture(t *testing.T) (*sqlite.Database, *gin.Engine) {
	t.Helper()
	return restFixtureWithConfig(t, DefaultConfig())
}

func restFixtureWithConfig(t *testing.T, config Config) (*sqlite.Database, *gin.Engine) {
	t.Helper()
	repo, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	for _, id := range []string{"alice", "bob"} {
		if err := repo.Accounts.CreatePasswordUser(t.Context(), auth.User{ID: id, Email: id + "@example.test"}, []byte("test-hash")); err != nil {
			t.Fatal(err)
		}
	}
	router := gin.New()
	Register(router.Group(Prefix), fixtureService(repo.RunStores()), fixtureApplication{}, slog.New(slog.DiscardHandler), config)
	return repo, router
}

func fixtureService(stores run.ServiceStores) *run.Service {
	resolve := func(context.Context, string, string) (run.Target, error) {
		return run.Target{Name: "demo", Provider: demo.Provider{}, ResolveAgent: func(context.Context, string) (run.Agent, error) {
			return demo.Agent{}, nil
		}}, nil
	}
	return run.NewService(stores, resolve, uuid.NewString, &run.Controls{}, nil)
}

func perform(t *testing.T, handler http.Handler, owner, method, path string, body any, want int) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequestWithContext(auth.WithUser(t.Context(), auth.User{ID: owner}), method, Prefix+path, bytes.NewReader(raw))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return serve(t, handler, request, want)
}

func serve(t *testing.T, handler http.Handler, request *http.Request, want int) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s = %d, want %d: %s", request.Method, request.URL, response.Code, want, response.Body)
	}
	return response
}

func decode[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func decodeTranscript(t *testing.T, response *httptest.ResponseRecorder) *sessionpb.TranscriptPage {
	t.Helper()
	if got := response.Header().Get("Content-Type"); got != httpserver.ProtobufMediaType {
		t.Fatalf("transcript content type = %s", got)
	}
	var page sessionpb.TranscriptPage
	if err := proto.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if !page.Complete || page.SandboxSettings == nil {
		t.Fatalf("incomplete transcript: %v", &page)
	}
	return &page
}

func checkCode(t *testing.T, response *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := response.Header().Get("X-Shinona-Error-Code"); got != want {
		t.Fatalf("error code = %s, want %s: %s", got, want, response.Body)
	}
}

func TestConversationLifecycleAndOwnership(t *testing.T) {
	repo, router := restFixture(t)
	created := decode[sessionSummary](t, perform(t, router, "alice", "POST", "/sessions", map[string]string{}, 201))
	if created.ID == "" || !created.DefaultTitle {
		t.Fatalf("created = %+v", created)
	}
	path := "/sessions/" + created.ID
	settings := decode[sandboxSettings](t, perform(t, router, "alice", "PUT", "/settings/sandbox/policy", map[string]string{"policy": "SHARED"}, 200))
	other := decode[sandboxSettings](t, perform(t, router, "bob", "GET", "/settings/sandbox", nil, 200))
	if settings.Selected == nil || *settings.Selected != sandbox.Shared || other.Selected == nil || *other.Selected != sandbox.Fresh {
		t.Fatalf("incorrect account policies: %+v, %+v", settings, other)
	}
	body := map[string]any{"prompt": "first prompt", "payload": map[string]bool{"arrowJs": true}}
	checkCode(t, perform(t, router, "bob", "POST", path+"/turns", body, 404), "SESSION_NOT_FOUND")
	first := decode[turnResponse](t, perform(t, router, "alice", "POST", path+"/turns", body, 202))
	if first.Status != run.Queued || first.Policy == nil || *first.Policy != sandbox.Shared {
		t.Fatalf("unexpected submitted turn: %+v", first)
	}
	checkCode(t, perform(t, router, "alice", "POST", path+"/turns", body, 409), "ACTIVE_TURN")
	now := time.Now().UTC()
	if _, err := repo.Conversations.ClaimNext(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Conversations.Complete(t.Context(), first.ID, run.Completion{Status: run.Succeeded, UpdatedAt: now,
		Result: run.Result{Output: "complete", Parts: []run.TurnPart{{Kind: "thinking", Text: "Review the request."}}}}); err != nil {
		t.Fatal(err)
	}
	response := perform(t, router, "alice", "GET", path+"/transcript", nil, 200)
	page := decodeTranscript(t, response)
	if page.Session == nil || page.Session.Title != "first prompt" || page.Session.DefaultTitle || page.Session.TurnCount != 1 ||
		!page.GetLastOptions().GetArrowJs() || len(page.Turns) != 1 || page.Turns[0].StartedAt == nil ||
		len(page.Turns[0].Parts) != 1 || page.Turns[0].Parts[0].Text != "Review the request." {
		t.Fatalf("unexpected transcript: %v", page)
	}
	hidden := decodeTranscript(t, perform(t, router, "bob", "GET", path+"/transcript", nil, 200))
	if hidden.Session != nil {
		t.Fatalf("another account received the transcript: %+v", hidden)
	}
	named := decode[sessionSummary](t, perform(t, router, "alice", "PATCH", path, map[string]string{"title": "Named session"}, 200))
	if named.DefaultTitle || named.Title != "Named session" {
		t.Fatalf("rename session: %+v", named)
	}
	renamed := decodeTranscript(t, perform(t, router, "alice", "GET", path+"/transcript", nil, 200))
	if renamed.Session == nil || renamed.Session.Title != "Named session" || len(renamed.Turns) != 1 {
		t.Fatalf("transcript reused an old session summary: %+v", renamed)
	}
	perform(t, router, "alice", "POST", path+"/turns/"+first.ID+"/revert", map[string]string{"expectedHeadId": first.ID}, 200)
	reverted := decodeTranscript(t, perform(t, router, "alice", "GET", path+"/transcript", nil, 200))
	if reverted.Session == nil || len(reverted.Turns) != 0 || reverted.Session.TurnCount != 0 {
		t.Fatalf("encoded cache retained a reverted turn: %+v", reverted)
	}
	perform(t, router, "alice", "DELETE", path, nil, 204)
}

func TestComposerDefaultsAreAccountScopedAndValidated(t *testing.T) {
	_, router := restFixture(t)
	read := func(owner string) *run.ComposerDefaults {
		return decode[ComposerOptions](t, perform(t, router, owner, "GET", "/composer/options", nil, 200)).Defaults
	}
	if read("alice") != nil {
		t.Fatal("a new account has saved defaults")
	}
	want := run.ComposerDefaults{Model: "fixture-model", Permission: "READ_ONLY", Thinking: "high"}
	saved := decode[run.ComposerDefaults](t, perform(t, router, "alice", "PUT", "/composer/defaults", want, 200))
	if saved != want {
		t.Fatalf("saved = %+v, want %+v", saved, want)
	}
	invalid := want
	invalid.Thinking = "unknown"
	checkCode(t, perform(t, router, "alice", "PUT", "/composer/defaults", invalid, 400), "INVALID_COMPOSER_OPTIONS")
	if got := read("alice"); got == nil || *got != want || read("bob") != nil {
		t.Fatalf("incorrectly persisted defaults: %+v", got)
	}
}

func TestProjectSessionsAndOwnership(t *testing.T) {
	_, router := restFixture(t)
	project := decode[projectResponse](t, perform(t, router, "alice", "POST", "/projects", map[string]string{"name": " Website  refresh "}, 201))
	if project.ID == "" || project.Name != "Website refresh" {
		t.Fatalf("created project: %+v", project)
	}
	checkCode(t, perform(t, router, "alice", "POST", "/projects", map[string]string{"name": project.Name}, 409), "PROJECT_NAME_EXISTS")
	checkCode(t, perform(t, router, "alice", "POST", "/projects", map[string]string{"name": "  "}, 400), "INVALID_PROJECT_NAME")

	projectArg := map[string]string{"projectId": project.ID}
	checkCode(t, perform(t, router, "bob", "POST", "/sessions", projectArg, 404), "PROJECT_NOT_FOUND")
	local := decode[sessionSummary](t, perform(t, router, "alice", "POST", "/sessions", map[string]string{}, 201))
	assigned := decode[sessionSummary](t, perform(t, router, "alice", "POST", "/sessions", projectArg, 201))
	all := decode[sessionsResponse](t, perform(t, router, "alice", "GET", "/sessions", nil, 200))
	if len(all.Projects) != 1 || all.Projects[0] != project || len(all.Sessions) != 2 ||
		local.ProjectID != nil || assigned.ProjectID == nil || *assigned.ProjectID != project.ID {
		t.Fatalf("list sessions: %+v", all)
	}
	for _, fixture := range []struct{ projectID, sessionID string }{{"", local.ID}, {project.ID, assigned.ID}} {
		path := "/sessions?projectId=" + url.QueryEscape(fixture.projectID)
		filtered := decode[sessionsResponse](t, perform(t, router, "alice", "GET", path, nil, 200))
		if len(filtered.Sessions) != 1 || filtered.Sessions[0].ID != fixture.sessionID {
			t.Fatalf("filter %q: %+v", fixture.projectID, filtered)
		}
		hidden := decode[sessionsResponse](t, perform(t, router, "bob", "GET", path, nil, 200))
		if len(hidden.Sessions) != 0 || len(hidden.Projects) != 0 {
			t.Fatalf("cross-account list: %+v", hidden)
		}
	}
	path := "/projects/" + project.ID
	name := map[string]string{"name": "Documentation refresh"}
	checkCode(t, perform(t, router, "bob", "PATCH", path, name, 404), "PROJECT_NOT_FOUND")
	if renamed := decode[projectResponse](t, perform(t, router, "alice", "PATCH", path, name, 200)); renamed.Name != name["name"] {
		t.Fatalf("project rename: %+v", renamed)
	}
	checkCode(t, perform(t, router, "bob", "DELETE", path, nil, 404), "PROJECT_NOT_FOUND")
	perform(t, router, "alice", "DELETE", path, nil, 204)
	moved := decode[sessionsResponse](t, perform(t, router, "alice", "GET", "/sessions?projectId=", nil, 200))
	if len(moved.Projects) != 0 || len(moved.Sessions) != 2 {
		t.Fatalf("sessions did not move to the default project: %+v", moved)
	}
}

func TestTranscriptPaginationAndHTTPCompression(t *testing.T) {
	repo, router := restFixture(t)
	now := time.Now().UTC()
	if err := repo.Conversations.CreateSession(t.Context(), run.Session{ID: "history", OwnerID: "alice", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i := range run.TranscriptPageSize + 1 {
		id := fmt.Sprintf("turn-%d", i+1)
		item := run.Run{ID: id, OwnerID: "alice", SessionID: "history", Status: run.Queued, CreatedAt: now, UpdatedAt: now}
		if _, err := repo.Conversations.Enqueue(t.Context(), item, "History", false); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Conversations.ClaimNext(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		turn := run.Result{Output: strings.Repeat("history result\n", 500)}
		if err := repo.Conversations.Complete(t.Context(), id, run.Completion{Result: turn, Status: run.Succeeded, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	const path = "/sessions/history/transcript"
	latest := decodeTranscript(t, perform(t, router, "alice", "GET", path, nil, 200))
	if latest.GetSession().GetTurnCount() != run.TranscriptPageSize+1 || len(latest.Turns) != run.TranscriptPageSize ||
		latest.Turns[0].Sequence != 2 || latest.NextBeforeSequence != 2 {
		t.Fatalf("latest page: %+v", latest)
	}
	ctx, recorder := timing.WithRecorder(auth.WithUser(t.Context(), auth.User{ID: "alice"}))
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, Prefix+path, nil)
	cached := decodeTranscript(t, serve(t, router, request, 200))
	if !proto.Equal(cached, latest) || !slices.ContainsFunc(recorder.Metrics(), func(metric timing.Metric) bool {
		return metric.Name == "transcript_response_cache_hit"
	}) {
		t.Fatalf("latest stable page did not return its cached representation: %+v", recorder.Metrics())
	}
	older := decodeTranscript(t, perform(t, router, "alice", "GET", path+"?beforeSequence=2", nil, 200))
	if len(older.Turns) != 1 || older.Turns[0].Sequence != 1 || older.NextBeforeSequence != 0 {
		t.Fatalf("older page: %+v", older)
	}
	checkCode(t, perform(t, router, "alice", "GET", path+"?paged=false&beforeSequence=2", nil, 400), "INVALID_TRANSCRIPT_CURSOR")

	plain := perform(t, router, "alice", "GET", path+"?paged=false", nil, 200)
	full := decodeTranscriptExport(t, plain, sessionpb.TranscriptExportEnd_NONE)
	if len(full) != run.TranscriptPageSize+3 || full[0].GetHeader().GetTurnCount() != run.TranscriptPageSize+1 ||
		full[1].GetTurn().GetSequence() != 1 || full[len(full)-2].GetTurn().GetSequence() != run.TranscriptPageSize+1 {
		t.Fatalf("full transcript did not return all %d turns in sequence", run.TranscriptPageSize+1)
	}
	for _, encoding := range []string{"br, zstd", "gzip", "zstd;q=0, *;q=1"} {
		request := httptest.NewRequestWithContext(auth.WithUser(t.Context(), auth.User{ID: "alice"}), http.MethodGet, Prefix+path+"?paged=false", nil)
		request.Header.Set("Accept-Encoding", encoding)
		got := serve(t, router, request, 200)
		if !slices.Equal(got.Header().Values("Vary"), []string{"Accept-Encoding"}) {
			t.Fatalf("unexpected Vary: %+v", got.Header())
		}
		raw := got.Body.Bytes()
		if encoding == "br, zstd" {
			if got.Header().Get("Content-Encoding") != "zstd" {
				t.Fatal("response did not use HTTP Zstandard")
			}
			reader, err := zstd.NewReader(nil, zstd.WithDecoderMaxWindow(8<<20))
			if err != nil {
				t.Fatal(err)
			}
			raw, err = reader.DecodeAll(raw, nil)
			reader.Close()
			if err != nil {
				t.Fatal(err)
			}
		} else if got.Header().Get("Content-Encoding") != "" {
			t.Fatal("an explicitly rejected encoding was used")
		}
		if !bytes.Equal(raw, plain.Body.Bytes()) {
			t.Fatal("compression changed the Protobuf representation")
		}
	}
	// Neither another session's write nor project metadata changes affect this transcript.
	perform(t, router, "alice", "POST", "/sessions", map[string]string{}, 201)
	perform(t, router, "alice", "POST", "/projects", map[string]string{"name": "Unrelated"}, 201)

	ctx, recorder = timing.WithRecorder(auth.WithUser(t.Context(), auth.User{ID: "alice"}))
	request = httptest.NewRequestWithContext(ctx, http.MethodGet, Prefix+path, nil)
	if page := decodeTranscript(t, serve(t, router, request, 200)); !proto.Equal(page, latest) ||
		!slices.ContainsFunc(recorder.Metrics(), func(metric timing.Metric) bool { return metric.Name == "transcript_response_cache_hit" }) {
		t.Fatalf("unrelated changes invalidated the paged transcript: %+v", recorder.Metrics())
	}

	ctx, recorder = timing.WithRecorder(auth.WithUser(t.Context(), auth.User{ID: "alice"}))
	request = httptest.NewRequestWithContext(ctx, http.MethodGet, Prefix+path+"?paged=false", nil)
	request.Header.Set("Accept-Encoding", "zstd")
	cachedResponse := serve(t, router, request, 200)
	if cachedResponse.Header().Get("Content-Encoding") != "zstd" ||
		!slices.ContainsFunc(recorder.Metrics(), func(metric timing.Metric) bool {
			return metric.Name == "transcript_response_cache_hit"
		}) {
		t.Fatalf("unrelated changes invalidated the compressed export: %+v", recorder.Metrics())
	}

	perform(t, router, "alice", "PATCH", "/sessions/history", map[string]string{"title": "Renamed history"}, 200)
	renamed := decodeTranscript(t, perform(t, router, "alice", "GET", path, nil, 200))
	if renamed.GetSession().GetTitle() != "Renamed history" {
		t.Fatal("transcript cache served a stale title")
	}
	if _, err := repo.Conversations.RevertTurn(t.Context(), "alice", "history", "turn-31", "turn-31", now); err != nil {
		t.Fatal(err)
	}
	replacement := run.Run{ID: "replacement", OwnerID: "alice", SessionID: "history", Status: run.Queued, CreatedAt: now, UpdatedAt: now}
	if _, err := repo.Conversations.Enqueue(t.Context(), replacement, "History", false); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Conversations.ClaimNext(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Conversations.Complete(t.Context(), replacement.ID, run.Completion{Status: run.Succeeded, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	replaced := decodeTranscript(t, perform(t, router, "alice", "GET", path, nil, 200))
	if !proto.Equal(replaced.GetSession(), renamed.GetSession()) || replaced.Turns[len(replaced.Turns)-1].GetId() != replacement.ID {
		t.Fatal("transcript cache served the old head after a replacement with the same summary")
	}
	perform(t, router, "alice", "DELETE", "/sessions/history", nil, http.StatusNoContent)
	archived := decodeTranscriptExport(t, perform(t, router, "alice", "GET", path+"?paged=false", nil, 200), sessionpb.TranscriptExportEnd_NONE)
	if len(archived) != 2 || archived[0].GetHeader().GetSession() != nil {
		t.Fatal("export cache served a transcript after archiving")
	}
}

func TestTranscriptCacheUsesStateFromTheEncodedSnapshot(t *testing.T) {
	repo, _ := restFixture(t)
	now := time.Now().UTC()
	const owner, id = "alice", "snapshot"
	session := run.Session{ID: id, OwnerID: owner, Title: "Original", CustomTitle: true, CreatedAt: now, UpdatedAt: now}
	if err := repo.Conversations.CreateSession(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	rename := func(title string) {
		t.Helper()
		if _, err := repo.Conversations.UpdateSession(t.Context(), owner, id, run.SessionChanges{Title: &title}); err != nil {
			t.Fatal(err)
		}
	}
	stores := repo.RunStores()
	// Change the summary after the preflight cache lookup, before loading the page.
	stores.Transcripts = changingTranscript{stores.Transcripts, sync.OnceFunc(func() { rename("Changed") })}
	router := gin.New()
	Register(router.Group(Prefix), fixtureService(stores), fixtureApplication{}, slog.New(slog.DiscardHandler), DefaultConfig())
	read := func(title string, wantHit bool) {
		t.Helper()
		ctx, recorder := timing.WithRecorder(auth.WithUser(t.Context(), auth.User{ID: owner}))
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, Prefix+"/sessions/"+id+"/transcript", nil)
		page := decodeTranscript(t, serve(t, router, req, 200))
		if got := page.GetSession().GetTitle(); got != title {
			t.Fatalf("title = %q, want %q", got, title)
		}
		if hit := slices.ContainsFunc(recorder.Metrics(), func(metric timing.Metric) bool {
			return metric.Name == "transcript_response_cache_hit"
		}); hit != wantHit {
			t.Fatalf("cache hit for %q = %t, want %t", title, hit, wantHit)
		}
	}
	read("Changed", false)
	read("Changed", true)
	rename("Original")
	read("Original", false)
}

func TestTranscriptResponsesWorkWithHTTPCachesDisabled(t *testing.T) {
	config := DefaultConfig()
	config.Transcript.ResponseCacheBytes, config.Transcript.TurnCacheBytes = 0, 0
	repo, router := restFixtureWithConfig(t, config)
	session := decode[sessionSummary](t, perform(t, router, "alice", http.MethodPost, "/sessions", map[string]string{}, 201))
	path := "/sessions/" + session.ID
	turn := decode[turnResponse](t, perform(t, router, "alice", http.MethodPost, path+"/turns", map[string]string{"prompt": "Run a turn."}, 202))
	now := time.Now().UTC()
	if _, err := repo.Conversations.ClaimNext(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Conversations.Complete(t.Context(), turn.ID, run.Completion{Status: run.Succeeded, UpdatedAt: now,
		Result: run.Result{Output: "Hello"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/transcript", "/transcript?paged=false"} {
		for range 2 {
			ctx, recorder := timing.WithRecorder(auth.WithUser(t.Context(), auth.User{ID: "alice"}))
			response := serve(t, router, httptest.NewRequestWithContext(ctx, http.MethodGet, Prefix+path+suffix, nil), 200)
			var result *sessionpb.Turn
			if suffix == "/transcript" {
				page := decodeTranscript(t, response)
				if len(page.Turns) != 1 {
					t.Fatalf("disabled cache returned %d turns", len(page.Turns))
				}
				result = page.Turns[0]
			} else {
				frames := decodeTranscriptExport(t, response, sessionpb.TranscriptExportEnd_NONE)
				if len(frames) != 3 {
					t.Fatalf("disabled cache returned %d export frames", len(frames))
				}
				result = frames[1].GetTurn()
			}
			metrics := recorder.Metrics()
			if result.GetOutput() != "Hello" || !slices.ContainsFunc(metrics, func(metric timing.Metric) bool {
				return metric.Name == "transcript_turn_encode"
			}) || slices.ContainsFunc(metrics, func(metric timing.Metric) bool {
				return metric.Name == "transcript_response_cache_hit"
			}) {
				t.Fatalf("uncached response output=%q metrics=%+v", result.GetOutput(), metrics)
			}
		}
	}
}

type changingTranscript struct {
	run.TranscriptRepository
	beforePage func()
}

func (r changingTranscript) TranscriptPage(ctx context.Context, owner, session string, before int) (*run.Transcript, error) {
	r.beforePage()
	return r.TranscriptRepository.TranscriptPage(ctx, owner, session, before)
}

type stalledExports struct {
	run.TranscriptRepository
	entered chan struct{}
}

func (s stalledExports) StreamTranscript(ctx context.Context, _, _ string, _ func(run.TranscriptStream) error) error {
	s.entered <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

func TestFullTranscriptQueueAllowsPagedReadsAndCancellation(t *testing.T) {
	repo, _ := restFixture(t)
	stores := repo.RunStores()
	entered := make(chan struct{}, 2)
	stores.Transcripts = stalledExports{stores.Transcripts, entered}
	router := gin.New()
	Register(router.Group(Prefix), fixtureService(stores), fixtureApplication{}, slog.New(slog.DiscardHandler),
		Config{Transcript: TranscriptLimits{Exports: 1, Pages: 2}})
	const path = "/sessions/history/transcript"
	ctx, cancel := context.WithCancel(auth.WithUser(t.Context(), auth.User{ID: "alice"}))
	defer cancel()
	full := httptest.NewRequestWithContext(ctx, http.MethodGet, Prefix+path+"?paged=false", nil)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		router.ServeHTTP(httptest.NewRecorder(), full)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first export did not acquire a slot")
	}

	pagedContext, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	paged := httptest.NewRequestWithContext(pagedContext, http.MethodGet, Prefix+path, nil)
	page := decodeTranscript(t, serve(t, router, paged, http.StatusOK))
	if page.Session != nil {
		t.Fatal("the full queue blocked the paged request")
	}

	waiting, stopWaiting := context.WithTimeout(ctx, 25*time.Millisecond)
	defer stopWaiting()
	second := httptest.NewRequestWithContext(waiting, http.MethodGet, Prefix+path+"?paged=false", nil)
	checkCode(t, serve(t, router, second, http.StatusGatewayTimeout), "TRANSCRIPT_EXPORT_TIMEOUT")
	select {
	case <-entered:
		t.Fatal("the second export entered despite a full queue")
	default:
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("the canceled request remained in the full queue")
	}
}
