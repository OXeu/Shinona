package main

import (
	"bufio"
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
	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/httpauth"
	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/adapter/password"
	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
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
	b.Run("SessionsListFiltered", benchmarkSessionsListFiltered)
	b.Run("SessionListPoll", benchmarkSessionListPoll)
	b.Run("WorkerScheduling", benchmarkWorkerScheduling)
	b.Run("Transcript", benchmarkTranscript)
	b.Run("TranscriptServer", benchmarkTranscriptServer)
	b.Run("TranscriptCold", func(b *testing.B) { benchmarkTranscriptCold(b, false) })
	b.Run("TranscriptWithImages", func(b *testing.B) { benchmarkTranscriptCold(b, true) })
	b.Run("TranscriptFullStream", benchmarkTranscriptFullStream)
	b.Run("Attachment", benchmarkAttachment)
	b.Run("AuthSession", benchmarkAuthSession)
	b.Run("LiveSync", benchmarkLiveSync)
	b.Run("LiveStream", benchmarkLiveStream)
	b.Run("Send", func(b *testing.B) { benchmarkSubmitRun(b, false) })
	b.Run("Queue", func(b *testing.B) { benchmarkSubmitRun(b, true) })
	b.Run("Steering", benchmarkSteering)
	b.Run("Revert", benchmarkRevert)
}

type benchmarkAPI struct {
	*httptest.Server
	repo         *sqlite.Database
	databasePath string
	service      *run.Service
	controls     *run.Controls
	resolve      run.ResolveTarget
	logger       *slog.Logger
	owner        string
	newHandler   func() http.Handler
	accounts     *auth.Service
}

func benchmarkRepository(b testing.TB) *sqlite.Database {
	b.Helper()
	return openBenchmarkDatabase(b, filepath.Join(b.TempDir(), "benchmark.db"))
}

func openBenchmarkDatabase(b testing.TB, path string) *sqlite.Database {
	b.Helper()
	repo, err := sqlite.Open(b.Context(), path)
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

func newBenchmarkAPI(b testing.TB, repo *sqlite.Database, target run.Target) *benchmarkAPI {
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
	service := run.NewService(repo.RunStores(), resolve, uuid.NewString, controls, nil)
	b.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := service.CloseTerminals(cleanup); err != nil {
			b.Error(err)
		}
	})
	accounts := auth.New(repo.Accounts, password.Argon2id{}, uuid.NewString)
	const email, secret = "benchmark@example.invalid", "local benchmark password"
	owner, err := accounts.CreatePasswordUser(ctx, email, secret)
	if err != nil {
		b.Fatal(err)
	}
	authHandler := httpauth.New(accounts, nil, false, logger)
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	newHandler := func() http.Handler {
		router := newAPIRouter(ctx, origin, service, nil, authHandler, logger, rest.DefaultConfig())
		return httpserver.ServerTiming(allowedHost(router, func() string { return "" }))
	}
	server.Config.Handler = newHandler()
	server.Start()
	b.Cleanup(server.Close)
	server.Client().Jar, err = cookiejar.New(nil)
	if err != nil {
		b.Fatal(err)
	}
	fixture := &benchmarkAPI{
		Server: server, repo: repo, service: service, controls: controls, resolve: resolve,
		logger: logger, owner: owner.ID, newHandler: newHandler, accounts: accounts,
	}
	fixture.post(b, "/auth/password", fixture.requestBody(b, map[string]string{"email": email, "password": secret}), http.StatusOK)
	return fixture
}

func newCoreBenchmarkAPI(b testing.TB, agent run.Agent) *benchmarkAPI {
	b.Helper()
	databasePath := filepath.Join(b.TempDir(), "benchmark.db")
	api := newBenchmarkAPI(b, openBenchmarkDatabase(b, databasePath), benchmarkTarget("demo", demo.Provider{}, agent))
	api.databasePath = databasePath
	return api
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

func (a *benchmarkAPI) post(b testing.TB, path string, body []byte, status int) benchmarkResponse {
	b.Helper()
	ctx, cancel := context.WithTimeout(b.Context(), run.ProjectSandboxTimeout+5*time.Second)
	defer cancel()
	response, err := a.measuredRequest(ctx, a.Client(), http.MethodPost, path, body, status, nil)
	if err != nil {
		b.Fatal(err)
	}
	return response
}

func (a *benchmarkAPI) request(ctx context.Context, client *http.Client, method, path string, body []byte, status int) ([]byte, http.Header, error) {
	response, err := a.measuredRequest(ctx, client, method, path, body, status, nil)
	return response.body, response.header, err
}

type benchmarkResponse struct {
	body            []byte
	header, trailer http.Header
	wireBytes       int
}

type benchmarkPayloadSize struct{ wire, body int64 }

func (s *benchmarkPayloadSize) add(response benchmarkResponse) {
	s.wire += int64(response.wireBytes)
	s.body += int64(len(response.body))
}

func (s benchmarkPayloadSize) report(b *testing.B) {
	b.ReportMetric(float64(s.wire)/float64(b.N), "wire-B/op")
	b.ReportMetric(float64(s.body)/float64(b.N), "body-B/op")
}

func (a *benchmarkAPI) measuredRequest(ctx context.Context, client *http.Client, method, path string, body []byte, status int, headers http.Header) (benchmarkResponse, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.URL+path, bytes.NewReader(body))
	if err != nil {
		return benchmarkResponse{}, err
	}
	if len(body) != 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Origin", a.URL)
	req.Header.Set("Accept-Encoding", "zstd")
	response, err := client.Do(req)
	if err != nil {
		return benchmarkResponse{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return benchmarkResponse{}, err
	}
	wireBytes := len(data)
	if response.Header.Get("Content-Encoding") == "zstd" {
		decoder := benchmarkZstdDecoders.Get().(*zstd.Decoder)
		data, err = decoder.DecodeAll(data, nil)
		benchmarkZstdDecoders.Put(decoder)
		if err != nil {
			return benchmarkResponse{}, err
		}
	}
	if response.StatusCode != status {
		return benchmarkResponse{}, fmt.Errorf("%s returned %d, want %d: %s", path, response.StatusCode, status, data)
	}
	return benchmarkResponse{data, response.Header, response.Trailer, wireBytes}, nil
}

func (a *benchmarkAPI) rest(b *testing.B, method, path string, body []byte, status int, result any) benchmarkResponse {
	b.Helper()
	ctx, cancel := context.WithTimeout(b.Context(), run.ProjectSandboxTimeout+5*time.Second)
	defer cancel()
	response, err := a.measuredRequest(ctx, a.Client(), method, path, body, status, nil)
	if err != nil {
		b.Fatal(err)
	}
	if result != nil {
		if err := json.Unmarshal(response.body, result); err != nil {
			b.Fatalf("decode %s: %v", path, err)
		}
	}
	return response
}

func benchmarkSessionPath(session string) string {
	return rest.Prefix + "/sessions/" + url.PathEscape(session)
}

// The caller owns the connection so timed reconnects can release it each iteration.
func (a *benchmarkAPI) dialBenchmarkSocket(b testing.TB, path string, dialer websocket.Dialer) *websocket.Conn {
	b.Helper()
	endpoint, err := url.Parse(a.URL)
	if err != nil {
		b.Fatal(err)
	}
	headers := http.Header{"Origin": {a.URL}}
	for _, cookie := range a.Client().Jar.Cookies(endpoint) {
		headers.Add("Cookie", cookie.String())
	}
	dialer.HandshakeTimeout = 5 * time.Second
	conn, response, err := dialer.DialContext(b.Context(), "ws://"+endpoint.Host+path, headers)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		b.Fatal(err)
	}
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
	return a.completedTurnWithPayload(b, session, prompt, run.Payload{}, result)
}

func (a *benchmarkAPI) completedTurnWithPayload(b testing.TB, session, prompt string, payload run.Payload, result run.Result) run.Run {
	b.Helper()
	ctx := b.Context()
	queued, err := a.service.Submit(ctx, a.owner, session, prompt, payload, false)
	if err != nil {
		b.Fatal(err)
	}
	item, err := a.repo.Conversations.ClaimNext(ctx, time.Now())
	if err != nil || item == nil || item.ID != queued.ID {
		b.Fatalf("claim fixture turn %s: %v, %v", queued.ID, item, err)
	}
	if err := a.repo.Conversations.Complete(ctx, item.ID, run.Completion{Result: result, Status: run.Succeeded, UpdatedAt: time.Now()}); err != nil {
		b.Fatal(err)
	}
	return *item
}

func benchmarkSessionsList(b *testing.B) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	projectCount := a.seedBenchmarkSessions(b, 256)
	request := a.recentBenchmarkClient(b, projectCount)
	benchmarkReadRequests(b, request)
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

type benchmarkRead func(context.Context) (benchmarkResponse, error)

func benchmarkReadRequests(b *testing.B, request benchmarkRead) {
	b.Helper()
	if _, err := request(b.Context()); err != nil {
		b.Fatal(err)
	}
	var size benchmarkPayloadSize
	for b.Loop() {
		response, err := request(b.Context())
		if err != nil {
			b.Fatal(err)
		}
		size.add(response)
	}
	size.report(b)
}

func (a *benchmarkAPI) recentBenchmarkClient(b testing.TB, projectCount int) benchmarkRead {
	b.Helper()
	return func(ctx context.Context) (benchmarkResponse, error) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		response, err := a.measuredRequest(ctx, a.Client(), http.MethodGet, rest.Prefix+"/sessions", nil, http.StatusOK, nil)
		if err != nil {
			return response, err
		}
		var page struct {
			Projects []run.Project
			Sessions []struct{ ID string }
		}
		if err := fastjson.Unmarshal(response.body, &page); err != nil {
			return response, err
		}
		if len(page.Projects) != projectCount || len(page.Sessions) != 30 || page.Sessions[0].ID == "" {
			return response, fmt.Errorf("unexpected recent response: %d projects, %d sessions", len(page.Projects), len(page.Sessions))
		}
		return response, nil
	}
}

func benchmarkTranscript(b *testing.B) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	const turnCount = 120
	session := a.seedBenchmarkTranscript(b, turnCount, 48)
	request := a.transcriptBenchmarkClient(b, a.Client(), session, turnCount, true)
	benchmarkReadRequests(b, request)
}

type benchmarkResponseWriter struct {
	header http.Header
	status int
	bytes  int
}

func (w *benchmarkResponseWriter) Header() http.Header { return w.header }
func (w *benchmarkResponseWriter) WriteHeader(status int) {
	w.status = status
}
func (w *benchmarkResponseWriter) Write(body []byte) (int, error) {
	w.bytes += len(body)
	return len(body), nil
}

func benchmarkTranscriptServer(b *testing.B) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	session := a.seedBenchmarkTranscript(b, 120, 48)
	request := a.authenticatedBenchmarkRequest(b, benchmarkSessionPath(session)+"/transcript")
	response := benchmarkResponseWriter{header: make(http.Header)}
	serve := func() {
		clear(response.header)
		response.status, response.bytes = http.StatusOK, 0
		a.Config.Handler.ServeHTTP(&response, request.Clone(b.Context()))
	}
	serve()
	if response.status != http.StatusOK || response.bytes == 0 ||
		response.header.Get("Content-Type") != httpserver.ProtobufMediaType ||
		response.header.Get("Content-Encoding") != "zstd" {
		b.Fatalf("unexpected response: %d, %d bytes, headers %v", response.status, response.bytes, response.header)
	}
	for b.Loop() {
		serve()
		if response.status != http.StatusOK {
			b.Fatalf("transcript returned %d", response.status)
		}
	}
	b.ReportMetric(float64(response.bytes), "wire-B/op")
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
	return func(ctx context.Context) (benchmarkResponse, error) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		response, err := a.measuredRequest(ctx, client, http.MethodGet, path, nil, http.StatusOK, nil)
		if err != nil {
			return response, err
		}
		if !paged {
			return response, verifyTranscriptExport(response, session, wantCount)
		}
		if response.header.Get("Content-Type") != httpserver.ProtobufMediaType {
			return response, fmt.Errorf("unexpected transcript content type: %s", response.header.Get("Content-Type"))
		}
		var page sessionpb.TranscriptPage
		if err := proto.Unmarshal(response.body, &page); err != nil {
			return response, err
		}
		if !page.Complete || page.SandboxSettings == nil || page.GetSession().GetId() != session || page.GetSession().GetTurnCount() != int32(turnCount) ||
			len(page.Turns) != wantCount || page.NextBeforeSequence != int32(wantCursor) {
			return response, fmt.Errorf("unexpected transcript page: turns=%d, cursor=%d, complete=%t", len(page.Turns), page.NextBeforeSequence, page.Complete)
		}
		return response, nil
	}
}

func verifyTranscriptExport(response benchmarkResponse, session string, turnCount int) error {
	if response.header.Get("Content-Type") != sessiontransfer.TranscriptExportMediaType {
		return fmt.Errorf("unexpected export content type: %s", response.header.Get("Content-Type"))
	}
	reader := bufio.NewReader(bytes.NewReader(response.body))
	var frame sessionpb.TranscriptExportFrame
	if err := protodelim.UnmarshalFrom(reader, &frame); err != nil {
		return err
	}
	header := frame.GetHeader()
	if header.GetSession().GetId() != session || header.GetSandboxSettings() == nil || header.GetTurnCount() != int32(turnCount) {
		return fmt.Errorf("unexpected export header: %s", header)
	}
	for i := range turnCount {
		if err := protodelim.UnmarshalFrom(reader, &frame); err != nil {
			return err
		}
		if frame.GetTurn().GetSequence() != int32(i+1) {
			return fmt.Errorf("unexpected export turn at %d", i+1)
		}
	}
	if err := protodelim.UnmarshalFrom(reader, &frame); err != nil {
		return err
	}
	if frame.GetEnd() == nil || frame.GetEnd().GetFailure() != sessionpb.TranscriptExportEnd_NONE {
		return fmt.Errorf("missing or failed export end frame: %v", frame.GetEnd())
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		return fmt.Errorf("unexpected data after the export end frame")
	}
	return nil
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
		response := a.rest(b, http.MethodPost, path, body, http.StatusAccepted, &result)
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
				slowestServerTiming = response.header.Values("Server-Timing")
			}
		}
		if err := a.repo.Conversations.StopQueued(b.Context(), a.owner, session, result.ID, time.Now()); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	if queue && len(latencies) > 0 {
		slices.Sort(latencies)
		count := len(latencies)
		b.ReportMetric(float64(latencies[(count+1)/2-1])/float64(time.Microsecond), "p50-us/op")
		b.ReportMetric(float64(latencies[(99*count+99)/100-1])/float64(time.Microsecond), "p99-us/op")
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
	item, err := a.repo.Conversations.ClaimNext(b.Context(), time.Now())
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
		run.NewWorker(a.repo.Conversations, a.repo.Projects, a.resolve, a.logger, a.controls, run.DefaultWorkerConcurrency).Serve(ctx)
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
