package main

import (
	"bytes"
	"context"
	"encoding/binary"
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
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/graphql"
	"github.com/OXeu/shinona/internal/adapter/httpauth"
	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/adapter/password"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

const (
	benchmarkPrompt        = "Review the workspace changes and summarize the test results."
	benchmarkSessionFields = "id projectId title defaultTitle turnCount updatedAt pinned active unread"
	benchmarkSubmit        = `mutation SubmitTurn($sessionId: ID!, $prompt: String!, $payload: RunPayloadInput!, $queue: Boolean!) {
		submitRun(sessionId: $sessionId, prompt: $prompt, payload: $payload, queue: $queue) {
			id sequence prompt status policy output createdAt startedAt updatedAt model attachments
			steering { text delivered } parts { kind text tool input output status estimatedTokens }
			contextTokens contextWindow liveContext { tokens window estimated } tokenUsage { input cached output }
		}
	}`
)

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
	api, err := graphql.NewHandler(service, nil, logger)
	if err != nil {
		b.Fatal(err)
	}
	transfer, err := sessiontransfer.New(service, logger)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = transfer.Close() })
	accounts := auth.New(repo, password.Argon2id{}, uuid.NewString)
	const email, secret = "benchmark@example.invalid", "local benchmark password"
	owner, err := accounts.CreatePasswordUser(ctx, email, secret)
	if err != nil {
		b.Fatal(err)
	}
	authHandler := httpauth.New(accounts, nil, false, logger)
	mux := http.NewServeMux()
	server := httptest.NewUnstartedServer(httpserver.ServerTiming(allowedHost(mux, func() string { return "" })))
	authHandler.Routes(mux, sameOriginJSON)
	registerRunRoutes(mux, ctx, "http://"+server.Listener.Addr().String(), service, api, transfer, authHandler, logger)
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
	data, headers, err := a.postRequest(ctx, a.Client(), path, body, status)
	if err != nil {
		b.Fatal(err)
	}
	return data, headers
}

func (a *benchmarkAPI) postRequest(ctx context.Context, client *http.Client, path string, body []byte, status int) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", a.URL)
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode != status {
		return nil, nil, fmt.Errorf("%s returned %d, want %d: %s", path, response.StatusCode, status, data)
	}
	return data, response.Header, nil
}

func (a *benchmarkAPI) operation(b *testing.B, query string, variables any) []byte {
	b.Helper()
	return a.requestBody(b, struct {
		Query     string `json:"query"`
		Variables any    `json:"variables"`
	}{query, variables})
}

func (a *benchmarkAPI) graphql(b *testing.B, body []byte, field string, result any) http.Header {
	b.Helper()
	data, headers := a.post(b, "/graphql", body, http.StatusOK)
	var envelope struct {
		Data   map[string]json.RawMessage
		Errors []json.RawMessage
	}
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Errors) > 0 {
		b.Fatalf("GraphQL %s failed: %s (%v)", field, data, err)
	}
	value := envelope.Data[field]
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		b.Fatalf("GraphQL %s returned no result: %s", field, data)
	}
	if err := json.Unmarshal(value, result); err != nil {
		b.Fatalf("decode GraphQL %s: %v", field, err)
	}
	return headers
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
	conn := a.websocket(b, sessiontransfer.RecentPath)
	payload, err := proto.Marshal(&sessionpb.RecentRequest{})
	if err != nil {
		b.Fatal(err)
	}
	var page sessionpb.RecentPage
	return func(context.Context) error {
		deadline := time.Now().Add(15 * time.Second)
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return err
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return err
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
			return err
		}
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("receive recent sessions: %w", err)
		}
		if kind != websocket.BinaryMessage {
			return fmt.Errorf("unexpected recent sessions message type: %d", kind)
		}
		decoded, err := io.ReadAll(brotli.NewReader(bytes.NewReader(data)))
		if err != nil {
			return err
		}
		if err := proto.Unmarshal(decoded, &page); err != nil {
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
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(decoder.Close)
	request := struct {
		ID    string `json:"id"`
		Paged *bool  `json:"paged,omitempty"`
	}{ID: session}
	wantCount, wantCursor := turnCount, 0
	if !paged {
		request.Paged = new(false)
	} else if turnCount > run.TranscriptPageSize {
		wantCount, wantCursor = run.TranscriptPageSize, turnCount-run.TranscriptPageSize+1
	}
	body := a.requestBody(b, request)
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		data, headers, err := a.postRequest(ctx, client, sessiontransfer.TranscriptPath, body, http.StatusOK)
		if err != nil {
			return err
		}
		if headers.Get("Content-Type") != "application/vnd.shinona.transcript.v1+protobuf" {
			return fmt.Errorf("unexpected transcript content type: %s", headers.Get("Content-Type"))
		}
		var count int
		var header *sessionpb.TranscriptHeader
		var done bool
		for len(data) >= 4 && !done {
			frameSize := int(binary.BigEndian.Uint32(data))
			data = data[4:]
			if frameSize <= 0 || frameSize > len(data) {
				return fmt.Errorf("invalid transcript frame size: %d", frameSize)
			}
			frame, err := decoder.DecodeAll(data[:frameSize], nil)
			if err != nil {
				return err
			}
			var chunk sessionpb.TranscriptChunk
			if err := proto.Unmarshal(frame, &chunk); err != nil {
				return err
			}
			if chunk.GetHeader() != nil {
				header = chunk.GetHeader()
			}
			count += len(chunk.GetTurns().GetTurns())
			done = chunk.GetDone()
			data = data[frameSize:]
		}
		if !done || len(data) != 0 || count != wantCount || header.GetSession().GetId() != session ||
			header.GetSession().GetTurnCount() != int32(turnCount) || header.GetNextBeforeSequence() != int32(wantCursor) {
			return fmt.Errorf("unexpected transcript page: turns=%d, cursor=%d, complete=%t", count, header.GetNextBeforeSequence(), done)
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
	body := a.operation(b, benchmarkSubmit, map[string]any{
		"sessionId": session, "prompt": benchmarkPrompt, "payload": struct{}{}, "queue": queue,
	})
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
		headers := a.graphql(b, body, "submitRun", &result)
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
	const query = `mutation Steer($sessionId: ID!, $runId: ID!, $prompt: String!) {
		steerRun(sessionId: $sessionId, runId: $runId, prompt: $prompt)
	}`
	prepare := func() []byte {
		session := a.session(b, "")
		active, err := a.service.Submit(b.Context(), a.owner, session, benchmarkPrompt, run.Payload{}, false)
		if err != nil {
			b.Fatal(err)
		}
		return a.operation(b, query, map[string]string{"sessionId": session, "runId": active.ID, "prompt": benchmarkPrompt})
	}
	body := prepare()
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
		var accepted bool
		a.graphql(b, body, "steerRun", &accepted)
		if !accepted {
			b.Fatal("steerRun rejected the steering message")
		}
		b.StopTimer()
		if delivered := input.Steering.Drain(); len(delivered) != 1 || delivered[0] != benchmarkPrompt {
			b.Fatalf("agent received unexpected steering: %v", delivered)
		}
		messages++
		// Delivered messages still count toward the per-turn limit.
		if messages == 8 {
			body = prepare()
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
	const mutation = `mutation RevertTurn($sessionId: ID!, $turnId: ID!, $expectedHeadId: ID!) {
		revertTurn(sessionId: $sessionId, turnId: $turnId, expectedHeadId: $expectedHeadId) {
			session { ` + benchmarkSessionFields + ` }
			prompt
			images { name mediaType data }
		}
	}`
	for b.Loop() {
		b.StopTimer()
		selected := a.completedTurn(b, session, benchmarkPrompt, run.Result{Output: "Tests passed."})
		a.completedTurn(b, session, "Check the diagnostics", run.Result{Output: "No errors."})
		head := a.completedTurn(b, session, "Show the summary", run.Result{Output: "Ready for review."})
		body := a.operation(b, mutation, map[string]string{
			"sessionId": session, "turnId": selected.ID, "expectedHeadId": head.ID,
		})
		b.StartTimer()

		var restored struct {
			Session struct {
				ID        string
				TurnCount int
			}
			Prompt string
		}
		a.graphql(b, body, "revertTurn", &restored)
		if restored.Session.ID != session || restored.Session.TurnCount != 27 || restored.Prompt != benchmarkPrompt {
			b.Fatalf("unexpected revert draft: %+v", restored)
		}
	}
}
