//go:build linux

package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/graphql"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

const (
	transferOwner   = "benchmark-owner"
	transferSession = "session-00000"
	sessionPageSize = 30
	turnPageSize    = 50
	streamBatchSize = 8
)

type transferFixture struct {
	sessions, turns, textBytes int
}

var transferFixtures = map[string]transferFixture{
	"small": {sessions: 120, turns: 120, textBytes: 1024},
	"grown": {sessions: 8000, turns: 2000, textBytes: 8192},
}

func TestTransferPagesPreserveOrder(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture := transferFixtures["small"]
	fixture.seed(t, repo)

	sessions, err := repo.RecentSessions(ctx, transferOwner, nil, fixture.sessions)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := repo.Transcript(ctx, transferOwner, transferSession, fixture.turns)
	if err != nil || transcript == nil {
		t.Fatalf("load full fixture transcript: %v", err)
	}
	for resource, expected := range map[string][]any{
		"sessions": mapRows(sessions, summaryWire),
		"turns":    mapRows(transcript.Turns, turnWire),
	} {
		t.Run(resource, func(t *testing.T) {
			request := transferRequest{Resource: resource}
			var rows []any
			for {
				page, err := loadTransferPage(ctx, repo, request)
				if err != nil || len(page.Rows) == 0 {
					t.Fatalf("load page at %d: %d rows, %v", request.Offset, len(page.Rows), err)
				}
				if resource == "turns" {
					rows = append(page.Rows, rows...)
				} else {
					rows = append(rows, page.Rows...)
				}
				if page.NextOffset == nil {
					break
				}
				if *page.NextOffset <= request.Offset {
					t.Fatal("pagination offset did not advance")
				}
				request.Offset = *page.NextOffset
			}
			if !reflect.DeepEqual(rows, expected) {
				t.Fatal("paged records differ from the ordered fixture")
			}
		})
	}
}

// TestTransferBenchmarkServer is launched only by benchmarks/transfer/bench.js.
// The temporary listener and fixed account are never exposed by the Shinona server.
func TestTransferBenchmarkServer(t *testing.T) {
	profile := os.Getenv("SHINONA_TRANSFER_FIXTURE")
	if profile == "" {
		t.Skip("started by the transfer benchmark runner")
	}
	fixture, ok := transferFixtures[profile]
	if !ok {
		t.Fatalf("unknown fixture %q", profile)
	}

	repo, err := Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.seed(t, repo)
	target := func(context.Context, string, string) (run.Target, error) {
		return run.Target{Name: "demo", Provider: demo.Provider{}, ResolveAgent: func(context.Context, string) (run.Agent, error) { return demo.Agent{}, nil }}, nil
	}
	service := run.NewService(repo, target, nil, &run.Controls{}, nil)
	logger := slog.New(slog.DiscardHandler)
	baseline, err := graphql.NewHandler(service, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	assets := os.Getenv("SHINONA_TRANSFER_ASSETS")
	if assets == "" {
		t.Fatal("missing browser benchmark assets")
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
		_, _ = io.WriteString(w, "<!doctype html><title>Shinona transfer benchmark</title>")
	})
	mux.Handle("GET /bench/", http.StripPrefix("/bench/", http.FileServer(http.Dir(assets))))
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		baseline.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), auth.User{ID: transferOwner})))
	})
	mux.HandleFunc("GET /measure", transferMeasure)
	mux.HandleFunc("GET /ws", transferWebSocket(repo, false))
	mux.HandleFunc("GET /ws-ndjson", transferWebSocket(repo, true))
	mux.HandleFunc("POST /ndjson", transferStream(repo, false))
	mux.HandleFunc("POST /ai-sdk", transferStream(repo, true))
	mux.Handle(transferRPCPath, transferRPC(repo))
	registerMatrix(mux, repo)
	server := httptest.NewUnstartedServer(mux)
	defer server.Close()
	origin := "http://" + server.Listener.Addr().String()
	mqtt := startTransferMQTT(t, origin, repo)
	mux.HandleFunc("GET /mqtt-info", func(w http.ResponseWriter, _ *http.Request) {
		writeTransferJSON(w, mqtt)
	})
	server.Start()

	fmt.Printf("SHINONA_TRANSFER_URL=%s\n", server.URL)
	// The parent closes stdin when all benchmark cases finish.
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Error(err)
	}
}

func (f transferFixture) seed(t testing.TB, repo *Repository) {
	t.Helper()
	db := repo.db.WithContext(t.Context())
	epoch := time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)
	projectIDs := []string{"web", "backend", "docs", "operations"}
	for _, id := range projectIDs {
		if err := db.Create(&projectRow{ID: id, OwnerID: transferOwner, Name: "Project " + id}).Error; err != nil {
			t.Fatal(err)
		}
	}
	sessions := make([]sessionRow, f.sessions)
	turns := make([]row, 0, f.sessions+f.turns-1)
	for index := range sessions {
		id := fmt.Sprintf("session-%05d", index)
		created := epoch.Add(time.Duration(index) * time.Minute)
		session := sessionRow{ID: id, OwnerID: transferOwner, ProjectID: projectIDs[index%len(projectIDs)],
			Title:       fmt.Sprintf("Review implementation and CI results for issue %05d", index),
			CustomTitle: true, Pinned: index%127 == 0, TurnCount: 1, ReadThrough: index % 2,
			CreatedAt: created, UpdatedAt: created.Add(time.Second)}
		if index == 0 {
			session.ProjectID = ""
			session.TurnCount, session.ReadThrough = f.turns, f.turns
			session.UpdatedAt = created.Add(time.Duration(f.turns) * time.Second)
			for seq := 1; seq <= f.turns; seq++ {
				at := created.Add(time.Duration(seq) * time.Second)
				turns = append(turns, fixtureTurn(id, seq, at, f.textBytes))
			}
		} else {
			turns = append(turns, fixtureTurn(id, 1, created, 0))
			if index%311 == 0 {
				turns[len(turns)-1].Status = run.Running
			}
		}
		sessions[index] = session
	}
	if err := db.CreateInBatches(&sessions, 100).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.CreateInBatches(&turns, 50).Error; err != nil {
		t.Fatal(err)
	}
}

func fixtureTurn(sessionID string, seq int, at time.Time, textBytes int) row {
	item := row{
		ID: fmt.Sprintf("%s-turn-%05d", sessionID, seq), OwnerID: transferOwner, SessionID: sessionID,
		Sequence: seq, Policy: sandbox.Shared, Backend: "demo", Status: run.Succeeded,
		Prompt: fmt.Sprintf("Review checkpoint %05d and summarize the changes.", seq),
		Output: "Reviewed the requested change.", CreatedAt: at, StartedAt: &at, UpdatedAt: at,
		Payload: run.Payload{Options: run.Options{Model: "fixture-model"}}, ContextTokens: 1999 + seq, ContextWindow: 128000,
	}
	if textBytes > 0 {
		item.Output = fixtureText(seq, "Review and verification results", textBytes)
		item.Parts = []run.TurnPart{
			{Kind: "thinking", Text: fixtureText(seq, "Reasoning", 256)},
			{Kind: "tool", Tool: "bash", Input: `{"command":"go test ./..."}`,
				Output: fixtureText(seq, "Tool output", textBytes), Status: string(run.Succeeded)},
			{Kind: "text", Text: item.Output},
		}
		item.Payload.Images = []run.Image{{Name: fmt.Sprintf("capture-%05d.png", seq), MediaType: "image/png"}}
		if seq%7 == 0 {
			item.Steering = []run.SteeringMessage{{Text: "Include any failures.", Delivered: true}}
		}
	}
	return item
}

func fixtureText(seq int, heading string, size int) string {
	var builder strings.Builder
	builder.Grow(size + 128)
	fmt.Fprintf(&builder, "## %s for checkpoint %05d\n\n", heading, seq)
	for line := 0; builder.Len() < size; line++ {
		fmt.Fprintf(&builder, "- File %04d: inspected the patch, verified its output, and recorded result %08x.\n",
			line, uint32(seq)*2654435761+uint32(line)*2246822519)
	}
	return builder.String()[:size]
}

type transferRequest struct {
	Resource string `json:"resource"`
	Offset   int    `json:"offset"`
}

type transferPage struct {
	Meta       any   `json:"meta"`
	Rows       []any `json:"rows"`
	NextOffset *int  `json:"nextOffset"`
}

func loadTransferPage(ctx context.Context, repo *Repository, request transferRequest) (transferPage, error) {
	records, err := loadTransferRecords(ctx, repo, request)
	if err != nil {
		return transferPage{}, err
	}
	page := transferPage{NextOffset: records.nextOffset}
	if records.transcript == nil {
		page.Meta = map[string]any{"projects": mapRows(records.projects, func(project run.Project) any {
			return map[string]string{"id": project.ID, "name": project.Name}
		})}
		page.Rows = mapRows(records.sessions, summaryWire)
	} else {
		page.Meta = map[string]any{
			"session":         summaryWire(records.transcript.Session),
			"sandboxSettings": map[string]any{"available": []sandbox.Policy{sandbox.Fresh, sandbox.Shared}, "selected": sandbox.Fresh},
		}
		page.Rows = mapRows(records.transcript.Turns, turnWire)
	}
	return page, nil
}

type transferRecords struct {
	projects   []run.Project
	sessions   []run.Session
	transcript *run.Transcript
	nextOffset *int
}

func loadTransferRecords(ctx context.Context, repo *Repository, request transferRequest) (transferRecords, error) {
	var records transferRecords
	if request.Offset < 0 || request.Offset > 10000 {
		return records, fmt.Errorf("invalid page offset")
	}
	switch request.Resource {
	case "sessions":
		sessions, err := repo.recentSessions(ctx, transferOwner, nil, sessionPageSize+1, request.Offset)
		if err != nil {
			return records, err
		}
		projects, err := repo.Projects(ctx, transferOwner)
		if err != nil {
			return records, err
		}
		if len(sessions) > sessionPageSize {
			records.nextOffset = new(request.Offset + sessionPageSize)
			sessions = sessions[:sessionPageSize]
		}
		records.projects, records.sessions = projects, sessions
	case "turns":
		transcript, err := repo.transcript(ctx, transferOwner, transferSession, transcriptWindow{limit: turnPageSize + 1, offset: request.Offset})
		if err != nil {
			return records, fmt.Errorf("load benchmark transcript: %w", err)
		}
		if transcript == nil {
			return records, fmt.Errorf("benchmark session %s is missing", transferSession)
		}
		if len(transcript.Turns) > turnPageSize {
			records.nextOffset = new(request.Offset + turnPageSize)
			transcript.Turns = transcript.Turns[1:]
		}
		records.transcript = transcript
	default:
		return records, fmt.Errorf("unknown resource %q", request.Resource)
	}
	return records, nil
}

func mapRows[T any](items []T, convert func(T) any) []any {
	rows := make([]any, len(items))
	for i, item := range items {
		rows[i] = convert(item)
	}
	return rows
}

func summaryWire(s run.Session) any {
	var projectID *string
	if s.ProjectID != "" {
		projectID = &s.ProjectID
	}
	return map[string]any{
		"id": s.ID, "projectId": projectID, "title": s.Title, "defaultTitle": s.TurnCount == 0 && !s.CustomTitle,
		"turnCount": s.TurnCount, "updatedAt": s.UpdatedAt.Format(time.RFC3339Nano),
		"pinned": s.Pinned, "active": s.Active, "unread": s.Unread,
	}
}

func turnWire(turn run.Run) any {
	// Match the browser's GraphQL selections, including empty part fields.
	return map[string]any{
		"id": turn.ID, "sequence": turn.Sequence, "prompt": turn.Prompt, "status": turn.Status, "policy": turn.Policy,
		"output": turn.Output, "createdAt": turn.CreatedAt, "startedAt": turn.StartedAt, "updatedAt": turn.UpdatedAt,
		"model": turn.Payload.Options.Model, "contextTokens": turn.ContextTokens, "contextWindow": turn.ContextWindow,
		"attachments": mapRows(turn.Payload.Images, func(image run.Image) any { return image.Name }),
		"steering": mapRows(turn.Steering, func(message run.SteeringMessage) any {
			return map[string]any{"text": message.Text, "delivered": message.Delivered}
		}),
		"parts": mapRows(turn.Parts, func(part run.TurnPart) any {
			return map[string]string{"kind": part.Kind, "text": part.Text, "tool": part.Tool,
				"input": part.Input, "output": part.Output, "status": part.Status}
		}),
	}
}

func transferMeasure(w http.ResponseWriter, _ *http.Request) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		http.Error(w, "CPU accounting unavailable", http.StatusInternalServerError)
		return
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	writeTransferJSON(w, map[string]any{
		"cpuMicros":     (usage.Utime.Nano() + usage.Stime.Nano()) / 1000,
		"heapAllocated": memory.TotalAlloc,
	})
}

func writeTransferJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func transferWebSocket(repo *Repository, ndjson bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		conn.SetReadLimit(4096)
		for {
			var request transferRequest
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			page, err := loadTransferPage(r.Context(), repo, request)
			if err != nil {
				_ = conn.WriteJSON(map[string]string{"error": err.Error()})
				return
			}
			if ndjson {
				err = writeWebSocketNDJSON(conn, page)
			} else {
				err = conn.WriteJSON(page)
			}
			if err != nil {
				return
			}
		}
	}
}

func writeWebSocketNDJSON(conn *websocket.Conn, page transferPage) error {
	var batch bytes.Buffer
	encoder := json.NewEncoder(&batch)
	if err := encoder.Encode(map[string]any{"type": "data-page", "data": streamMetadata(page)}); err != nil {
		return err
	}
	for index, row := range page.Rows {
		if err := encoder.Encode(map[string]any{"type": "data-row", "data": row}); err != nil {
			return err
		}
		if (index+1)%streamBatchSize == 0 && index+1 < len(page.Rows) {
			if err := conn.WriteMessage(websocket.TextMessage, batch.Bytes()); err != nil {
				return err
			}
			batch.Reset()
		}
	}
	if err := encoder.Encode(map[string]string{"type": "data-end"}); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, batch.Bytes())
}

func streamMetadata(page transferPage) map[string]any {
	return map[string]any{"meta": page.Meta, "nextOffset": page.NextOffset}
}

func transferStream(repo *Repository, aiSDK bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request transferRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil {
			http.Error(w, "invalid page request", http.StatusBadRequest)
			return
		}
		page, err := loadTransferPage(r.Context(), repo, request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := writeTransferStream(w, page, aiSDK); err != nil && r.Context().Err() == nil {
			slog.Debug("write transfer stream", "error", err)
		}
	}
}

func writeTransferStream(w http.ResponseWriter, page transferPage, aiSDK bool) error {
	encoder := json.NewEncoder(w)
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/x-ndjson")
	if aiSDK {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-vercel-ai-ui-message-stream", "v1")
	}
	send := func(kind string, value any) error {
		if aiSDK {
			event, err := json.Marshal(map[string]any{"type": kind, "data": value, "transient": true})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(w, "data: %s\n\n", event)
			return err
		}
		return encoder.Encode(map[string]any{"type": kind, "data": value})
	}

	if err := send("data-page", streamMetadata(page)); err != nil {
		return err
	}
	for chunk := range slices.Chunk(page.Rows, streamBatchSize) {
		for _, row := range chunk {
			if err := send("data-row", row); err != nil {
				return err
			}
		}
		if err := controller.Flush(); err != nil {
			return err
		}
	}
	if aiSDK {
		_, err := io.WriteString(w, "data: [DONE]\n\n")
		return err
	}
	return nil
}
