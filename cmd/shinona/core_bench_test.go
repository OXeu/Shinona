package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	fastjson "github.com/segmentio/encoding/json"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/httpauth"
	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/adapter/password"
	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

const benchmarkPrompt = "Review the workspace changes and summarize the test results."

var benchmarkZstdDecoders = sync.Pool{New: func() any {
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		panic(err)
	}
	return decoder
}}

func BenchmarkCoreAPI(b *testing.B) {
	b.Run("SessionsList", benchmarkSessionsList)
	b.Run("Transcript", benchmarkTranscript)
	b.Run("Send", func(b *testing.B) { benchmarkSubmitRun(b, false) })
	b.Run("Queue", func(b *testing.B) { benchmarkSubmitRun(b, true) })
	b.Run("Steering", benchmarkSteering)
	b.Run("Revert", benchmarkRevert)
}

type benchmarkAPI struct {
	*httptest.Server
	repo     *sqlite.Repository
	service  *run.Service
	controls *run.Controls
	resolve  run.ResolveTarget
	logger   *slog.Logger
	owner    string
}

func benchmarkRepository(b testing.TB) *sqlite.Repository {
	b.Helper()
	repo, err := sqlite.Open(b.Context(), filepath.Join(b.TempDir(), "benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := repo.Close(); err != nil {
			b.Error(err)
		}
	})
	return repo
}

func newBenchmarkAPI(b testing.TB, repo *sqlite.Repository, target run.Target) *benchmarkAPI {
	b.Helper()
	ctx := b.Context()
	logger := slog.New(slog.DiscardHandler)
	controls := &run.Controls{}
	resolve := func(_ context.Context, _, name string) (run.Target, error) {
		if name != "" && name != target.Name {
			return run.Target{}, run.ErrSandboxProvider
		}
		return target, nil
	}
	service := run.NewService(repo, resolve, uuid.NewString, controls, nil)
	b.Cleanup(service.CloseTerminals)
	accounts := auth.New(repo, password.Argon2id{}, uuid.NewString)
	const email, secret = "benchmark@example.invalid", "local benchmark password"
	owner, err := accounts.CreatePasswordUser(ctx, email, secret)
	if err != nil {
		b.Fatal(err)
	}
	authHandler := httpauth.New(accounts, nil, false, logger)
	server := httptest.NewUnstartedServer(nil)
	router := newAPIRouter(ctx, "http://"+server.Listener.Addr().String(), service, nil, authHandler, logger)
	server.Config.Handler = httpserver.ServerTiming(allowedHost(router, func() string { return "" }))
	server.Start()
	b.Cleanup(server.Close)
	server.Client().Jar, err = cookiejar.New(nil)
	if err != nil {
		b.Fatal(err)
	}
	fixture := &benchmarkAPI{server, repo, service, controls, resolve, logger, owner.ID}
	fixture.post(b, "/auth/password", fixture.requestBody(b, map[string]string{"email": email, "password": secret}), http.StatusOK)
	return fixture
}

func newCoreBenchmarkAPI(b testing.TB, agent run.Agent) *benchmarkAPI {
	b.Helper()
	return newBenchmarkAPI(b, benchmarkRepository(b), benchmarkTarget("demo", demo.Provider{}, agent))
}

func benchmarkTarget(name string, provider sandbox.Provider, agent run.Agent) run.Target {
	return run.Target{
		Name: name, Provider: provider,
		ResolveAgent: func(context.Context, string) (run.Agent, error) { return agent, nil },
	}
}

func (*benchmarkAPI) requestBody(b testing.TB, value any) []byte {
	b.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		b.Fatal(err)
	}
	return body
}

func (a *benchmarkAPI) post(b testing.TB, path string, body []byte, status int) ([]byte, http.Header) {
	b.Helper()
	ctx, cancel := context.WithTimeout(b.Context(), run.ProjectSandboxTimeout+5*time.Second)
	defer cancel()
	data, headers, err := a.request(ctx, a.Client(), http.MethodPost, path, body, status)
	if err != nil {
		b.Fatal(err)
	}
	return data, headers
}

func (a *benchmarkAPI) request(ctx context.Context, client *http.Client, method, path string, body []byte, status int) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.URL+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if len(body) != 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", a.URL)
	req.Header.Set("Accept-Encoding", "zstd")
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, nil, err
	}
	if response.Header.Get("Content-Encoding") == "zstd" {
		decoder := benchmarkZstdDecoders.Get().(*zstd.Decoder)
		data, err = decoder.DecodeAll(data, nil)
		benchmarkZstdDecoders.Put(decoder)
		if err != nil {
			return nil, nil, err
		}
	}
	if response.StatusCode != status {
		return nil, nil, fmt.Errorf("%s returned %d, want %d: %s", path, response.StatusCode, status, data)
	}
	return data, response.Header, nil
}

func (a *benchmarkAPI) rest(b *testing.B, method, path string, body []byte, status int, result any) http.Header {
	b.Helper()
	ctx, cancel := context.WithTimeout(b.Context(), run.ProjectSandboxTimeout+5*time.Second)
	defer cancel()
	data, headers, err := a.request(ctx, a.Client(), method, path, body, status)
	if err != nil {
		b.Fatal(err)
	}
	if result != nil {
		if err := json.Unmarshal(data, result); err != nil {
			b.Fatalf("decode %s: %v", path, err)
		}
	}
	return headers
}

func benchmarkSessionPath(session string) string {
	return rest.Prefix + "/sessions/" + url.PathEscape(session)
}

func (a *benchmarkAPI) websocket(b testing.TB, path string) *websocket.Conn {
	b.Helper()
	endpoint, err := url.Parse(a.URL)
	if err != nil {
		b.Fatal(err)
	}
	headers := http.Header{"Origin": {a.URL}}
	for _, cookie := range a.Client().Jar.Cookies(endpoint) {
		headers.Add("Cookie", cookie.String())
	}
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, response, err := dialer.DialContext(b.Context(), "ws://"+endpoint.Host+path, headers)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = conn.Close() })
	return conn
}

func benchmarkSocketWrite(b *testing.B, conn *websocket.Conn, payload []byte) {
	b.Helper()
	if err := conn.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
		b.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		b.Fatal(err)
	}
}

func (a *benchmarkAPI) session(b testing.TB, project string) string {
	b.Helper()
	session, err := a.service.CreateSession(b.Context(), a.owner, project)
	if err != nil {
		b.Fatal(err)
	}
	return session.ID
}

func (a *benchmarkAPI) completedTurn(b testing.TB, session, prompt string, result run.Result) run.Run {
	b.Helper()
	ctx := b.Context()
	queued, err := a.service.Submit(ctx, a.owner, session, prompt, run.Payload{}, false)
	if err != nil {
		b.Fatal(err)
	}
	item, err := a.repo.ClaimNext(ctx, time.Now())
	if err != nil || item == nil || item.ID != queued.ID {
		b.Fatalf("claim fixture turn %s: %v, %v", queued.ID, item, err)
	}
	if err := a.repo.Complete(ctx, item.ID, run.Completion{Result: result, Status: run.Succeeded, UpdatedAt: time.Now()}); err != nil {
		b.Fatal(err)
	}
	return *item
}

func benchmarkSessionsList(b *testing.B) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	projectCount := a.seedBenchmarkSessions(b, 256)
	request := a.recentBenchmarkClient(b, projectCount)
	if err := request(b.Context()); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if err := request(b.Context()); err != nil {
			b.Fatal(err)
		}
	}
}

func (a *benchmarkAPI) seedBenchmarkSessions(b testing.TB, count int) int {
	b.Helper()
	projects := make([]run.Project, 4)
	for i := range projects {
		project, err := a.service.CreateProject(b.Context(), a.owner, fmt.Sprintf("Fixture project %d", i+1))
		if err != nil {
			b.Fatal(err)
		}
		projects[i] = project
	}
	for i := range count {
		session := a.session(b, projects[i%len(projects)].ID)
		a.completedTurn(b, session, fmt.Sprintf("Review workspace changes in feature %03d", i), run.Result{Output: "Reviewed the workspace."})
	}
	return len(projects)
}

type benchmarkRead func(context.Context) error

func (a *benchmarkAPI) recentBenchmarkClient(b testing.TB, projectCount int) benchmarkRead {
	b.Helper()
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		data, _, err := a.request(ctx, a.Client(), http.MethodGet, rest.Prefix+"/sessions", nil, http.StatusOK)
		if err != nil {
			return err
		}
		var page struct {
			Projects []run.Project
			Sessions []struct {
				ID, Title string
				TurnCount int
			}
		}
		if err := fastjson.Unmarshal(data, &page); err != nil {
			return err
		}
		if len(page.Projects) != projectCount || len(page.Sessions) != 30 || page.Sessions[0].TurnCount != 1 {
			return fmt.Errorf("unexpected recent response: %d projects, %d sessions", len(page.Projects), len(page.Sessions))
		}
		return nil
	}
}

func benchmarkTranscript(b *testing.B) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	const turnCount = 120
	session := a.seedBenchmarkTranscript(b, turnCount, 48)
	request := a.transcriptBenchmarkClient(b, a.Client(), session, turnCount, true)
	if err := request(b.Context()); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if err := request(b.Context()); err != nil {
			b.Fatal(err)
		}
	}
}

func (a *benchmarkAPI) seedBenchmarkTranscript(b testing.TB, turnCount, logLines int) string {
	b.Helper()
	session := a.session(b, "")
	for i := range turnCount {
		var log strings.Builder
		for line := range logLines {
			fmt.Fprintf(&log, "ok workspace/component-%02d test case %03d in batch %d\n", line%7, line, i+1)
		}
		a.completedTurn(b, session, benchmarkPrompt, run.Result{
			Output: "Tests passed. The workspace has pending changes.",
			Parts: []run.TurnPart{
				{Kind: "reasoning", Text: "Inspect the repository and run the tests."},
				{Kind: "tool", Tool: "bash", Input: "go test ./...", Output: log.String()},
				{Kind: "text", Text: "All tests passed."},
			},
		})
	}
	return session
}

func (a *benchmarkAPI) transcriptBenchmarkClient(b testing.TB, client *http.Client, session string, turnCount int, paged bool) benchmarkRead {
	b.Helper()
	path := benchmarkSessionPath(session) + "/transcript"
	wantCount, wantCursor := turnCount, 0
	if !paged {
		path += "?paged=false"
	} else if turnCount > run.TranscriptPageSize {
		wantCount, wantCursor = run.TranscriptPageSize, turnCount-run.TranscriptPageSize+1
	}
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		data, headers, err := a.request(ctx, client, http.MethodGet, path, nil, http.StatusOK)
		if err != nil {
			return err
		}
		if headers.Get("Content-Type") != httpserver.ProtobufMediaType {
			return fmt.Errorf("unexpected transcript content type: %s", headers.Get("Content-Type"))
		}
		var page sessionpb.TranscriptPage
		if err := proto.Unmarshal(data, &page); err != nil {
			return err
		}
		if !page.Complete || page.SandboxSettings == nil || page.GetSession().GetId() != session || page.GetSession().GetTurnCount() != int32(turnCount) ||
			len(page.Turns) != wantCount || page.NextBeforeSequence != int32(wantCursor) {
			return fmt.Errorf("unexpected transcript page: turns=%d, cursor=%d, complete=%t", len(page.Turns), page.NextBeforeSequence, page.Complete)
		}
		return nil
	}
}

func benchmarkSubmitRun(b *testing.B, queue bool) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	session := a.session(b, "")
	if queue {
		a.claimTurn(b, session)
	}
	path := benchmarkSessionPath(session) + "/turns"
	body := a.requestBody(b, map[string]any{"prompt": benchmarkPrompt, "payload": struct{}{}, "queue": queue})
	var latencies []time.Duration
	var slowest time.Duration
	var slowestServerTiming []string
	for b.Loop() {
		var result struct {
			ID     string
			Status run.Status
		}
		var elapsed time.Duration
		var started time.Time
		if queue {
			started = time.Now()
		}
		headers := a.rest(b, http.MethodPost, path, body, http.StatusAccepted, &result)
		if queue {
			elapsed = time.Since(started)
		}
		if result.ID == "" || result.Status != run.Queued {
			b.Fatalf("unexpected submitRun response: %+v", result)
		}
		b.StopTimer()
		if queue {
			latencies = append(latencies, elapsed)
			if elapsed > slowest {
				slowest = elapsed
				slowestServerTiming = headers.Values("Server-Timing")
			}
		}
		if err := a.repo.StopQueued(b.Context(), a.owner, session, result.ID, time.Now()); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	if queue && len(latencies) > 0 {
		slices.Sort(latencies)
		count := len(latencies)
		b.Logf("queue latency across %d requests: p50 %s, p99 %s, max %s; slowest Server-Timing: %s",
			count, latencies[(count+1)/2-1], latencies[(99*count+99)/100-1], slowest,
			strings.Join(slowestServerTiming, ", "))
	}
}

func (a *benchmarkAPI) claimTurn(b *testing.B, session string) {
	b.Helper()
	if _, err := a.service.Submit(b.Context(), a.owner, session, benchmarkPrompt, run.Payload{}, false); err != nil {
		b.Fatal(err)
	}
	item, err := a.repo.ClaimNext(b.Context(), time.Now())
	if err != nil || item == nil || item.SessionID != session {
		b.Fatalf("claim active turn: %v, %v", item, err)
	}
}

type benchmarkSteeringAgent struct {
	input   chan run.Input
	release chan struct{}
}

func (a benchmarkSteeringAgent) Run(ctx context.Context, input run.Input, _ sandbox.Workspace) (run.Result, error) {
	select {
	case a.input <- input:
	case <-ctx.Done():
	}
	select {
	case <-a.release:
		return run.Result{}, nil
	case <-ctx.Done():
		return run.Result{}, ctx.Err()
	}
}

func benchmarkSteering(b *testing.B) {
	agent := benchmarkSteeringAgent{input: make(chan run.Input, 1), release: make(chan struct{})}
	a := newCoreBenchmarkAPI(b, agent)
	body := a.requestBody(b, map[string]string{"prompt": benchmarkPrompt})
	prepare := func() string {
		session := a.session(b, "")
		active, err := a.service.Submit(b.Context(), a.owner, session, benchmarkPrompt, run.Payload{}, false)
		if err != nil {
			b.Fatal(err)
		}
		return benchmarkSessionPath(session) + "/turns/" + url.PathEscape(active.ID) + "/steering"
	}
	path := prepare()
	ctx, cancel := context.WithCancel(b.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.NewWorker(a.repo, a.resolve, a.logger, a.controls).Serve(ctx)
	}()
	b.Cleanup(func() {
		cancel()
		<-done
	})
	waitForTurn := func() run.Input {
		select {
		case input := <-agent.input:
			return input
		case <-time.After(5 * time.Second):
			b.Fatal("worker did not start the active turn")
			return run.Input{}
		}
	}
	input := waitForTurn()
	var messages int
	for b.Loop() {
		a.rest(b, http.MethodPost, path, body, http.StatusNoContent, nil)
		b.StopTimer()
		if delivered := input.Steering.Drain(); len(delivered) != 1 || delivered[0] != benchmarkPrompt {
			b.Fatalf("agent received unexpected steering: %v", delivered)
		}
		messages++
		// Delivered messages still count toward the per-turn limit.
		if messages == 8 {
			path = prepare()
			select {
			case agent.release <- struct{}{}:
			case <-time.After(5 * time.Second):
				b.Fatal("worker did not release the previous turn")
			}
			input = waitForTurn()
			messages = 0
		}
		b.StartTimer()
	}
}

func benchmarkRevert(b *testing.B) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	session := a.session(b, "")
	for range 27 {
		a.completedTurn(b, session, benchmarkPrompt, run.Result{Output: "Tests passed."})
	}
	for b.Loop() {
		b.StopTimer()
		selected := a.completedTurn(b, session, benchmarkPrompt, run.Result{Output: "Tests passed."})
		a.completedTurn(b, session, "Check the diagnostics", run.Result{Output: "No errors."})
		head := a.completedTurn(b, session, "Show the summary", run.Result{Output: "Ready for review."})
		path := benchmarkSessionPath(session) + "/turns/" + url.PathEscape(selected.ID) + "/revert"
		body := a.requestBody(b, map[string]string{"expectedHeadId": head.ID})
		b.StartTimer()

		var restored struct {
			Session struct {
				ID        string
				TurnCount int
			}
			Prompt string
		}
		a.rest(b, http.MethodPost, path, body, http.StatusOK, &restored)
		if restored.Session.ID != session || restored.Session.TurnCount != 27 || restored.Prompt != benchmarkPrompt {
			b.Fatalf("unexpected revert draft: %+v", restored)
		}
	}
}
