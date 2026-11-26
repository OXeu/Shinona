package sessiontransfer

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

type fixtureReader struct{}

func (fixtureReader) Projects(context.Context, string) ([]run.Project, error) {
	return []run.Project{{ID: "project", Name: "Project"}}, nil
}

func (fixtureReader) RecentSessions(_ context.Context, owner string, projectID *string) ([]run.Session, error) {
	sessions := []run.Session{{ID: owner + "-default", Title: "Default"}}
	if projectID == nil {
		sessions = append(sessions, run.Session{ID: owner + "-project", ProjectID: "project"})
	}
	return sessions, nil
}

func (fixtureReader) Settings(context.Context, string) (run.Settings, error) {
	return run.Settings{Selected: sandbox.Fresh, Available: []sandbox.Policy{sandbox.Fresh}}, nil
}

const fixtureUISpec = `{"root":"total","elements":{"total":{"type":"Metric","props":{"label":"Total","value":"42"}}}}`

func (fixtureReader) FullTranscript(_ context.Context, owner, id string) (*run.Transcript, error) {
	if id != owner {
		return nil, nil
	}
	turns := make([]run.Run, 10)
	for i := range turns {
		turns[i] = run.Run{Sequence: i + 1, Status: run.Succeeded, Output: "Demo reply",
			Payload: run.Payload{Images: []run.Image{{Name: "input.png", Data: "not included"}}}}
	}
	turns[0].TokenUsage = &run.TokenUsage{Input: 3_100, Cached: 2_122_000, Output: 30_210}
	turns[0].Parts = []run.TurnPart{{Kind: "ui", Text: "Total: 42", Output: fixtureUISpec}}
	turns[len(turns)-1].Payload.Options = run.Options{Model: "chosen-model", Permission: run.Workspace, Thinking: "high", MCP: "saved-mcp", ArrowJS: true}
	return &run.Transcript{Session: run.Session{ID: id, TurnCount: len(turns)}, Turns: turns}, nil
}

func (f fixtureReader) TranscriptPage(ctx context.Context, owner, id string, before int) (*run.Transcript, error) {
	page, err := f.FullTranscript(ctx, owner, id)
	if page != nil {
		end := len(page.Turns)
		if before > 0 {
			end = min(end, before-1)
		}
		start := max(0, end-5)
		page.Turns = page.Turns[start:end]
		if start > 0 {
			page.NextBeforeSequence = page.Turns[0].Sequence
		}
	}
	return page, err
}

func fixtureHandler(t *testing.T) *Handler {
	t.Helper()
	handler, err := New(fixtureReader{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	return handler
}

func TestRecentHonorsFilterAndRevocation(t *testing.T) {
	const origin = "https://shinona.example"
	var signedIn atomic.Bool
	signedIn.Store(true)
	handler := fixtureHandler(t).Recent(t.Context(), origin, func(*http.Request) (*auth.User, error) {
		if signedIn.Load() {
			return &auth.User{ID: "owner"}, nil
		}
		return nil, nil
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), auth.User{ID: "owner"})))
	}))
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	headers := http.Header{"Origin": {"https://other.example"}}
	conn, response, err := websocket.DefaultDialer.Dial(endpoint, headers)
	if conn != nil {
		_ = conn.Close()
	}
	if response != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin upgrade should be forbidden, got %v (%v)", response, err)
	}

	headers.Set("Origin", origin)
	conn, response, err = websocket.DefaultDialer.Dial(endpoint, headers)
	if response != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		project *string
		count   int
	}{{nil, 2}, {new(""), 1}} {
		request, err := proto.Marshal(&sessionpb.RecentRequest{ProjectId: tc.project})
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, request); err != nil {
			t.Fatal(err)
		}
		kind, compressed, err := conn.ReadMessage()
		if err != nil || kind != websocket.BinaryMessage {
			t.Fatalf("read binary session list: %d (%v)", kind, err)
		}
		payload, err := io.ReadAll(brotli.NewReader(bytes.NewReader(compressed)))
		if err != nil {
			t.Fatal(err)
		}
		var page sessionpb.RecentPage
		if err := proto.Unmarshal(payload, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Sessions) != tc.count || page.Sessions[0].GetId() != "owner-default" || page.Sessions[0].ProjectId != nil {
			t.Fatalf("unexpected session page: %v", &page)
		}
	}
	signedIn.Store(false)
	if err := conn.WriteMessage(websocket.BinaryMessage, nil); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("revoked connection returned %v", err)
	}
}

func TestTranscriptFramesAndOwnership(t *testing.T) {
	handler := fixtureHandler(t)
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	for _, tc := range []struct {
		owner, body       string
		first, last, next int32
		invalidCursor     bool
	}{
		{owner: "owner", body: `{"id":"owner","paged":false}`},
		{owner: "owner", body: `{"id":"owner"}`, first: 6, last: 10, next: 6},
		{owner: "other", body: `{"id":"owner","paged":true,"beforeSequence":6}`},
		{owner: "owner", body: `{"id":"owner","paged":true}`, first: 6, last: 10, next: 6},
		{owner: "owner", body: `{"id":"owner","beforeSequence":6}`, first: 1, last: 5},
		{owner: "owner", body: `{"id":"owner","paged":false,"beforeSequence":6}`, invalidCursor: true},
		{owner: "owner", body: `{"id":"owner","paged":true,"beforeSequence":0}`, invalidCursor: true},
	} {
		request := httptest.NewRequest(http.MethodPost, TranscriptPath, strings.NewReader(tc.body))
		request = request.WithContext(auth.WithUser(request.Context(), auth.User{ID: tc.owner}))
		response := httptest.NewRecorder()
		handler.Transcript(response, request)
		if tc.invalidCursor {
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid cursor: HTTP %d", response.Code)
			}
			continue
		}
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != transcriptType || !response.Flushed {
			t.Fatalf("unexpected transcript HTTP response: %d %v", response.Code, response.Header())
		}
		var chunks []*sessionpb.TranscriptChunk
		for response.Body.Len() != 0 {
			if response.Body.Len() < 4 {
				t.Fatal("truncated frame length")
			}
			size := int(binary.BigEndian.Uint32(response.Body.Next(4)))
			if size == 0 || size > response.Body.Len() {
				t.Fatalf("invalid frame length: %d", size)
			}
			payload, err := decoder.DecodeAll(response.Body.Next(size), nil)
			if err != nil {
				t.Fatal(err)
			}
			chunk := new(sessionpb.TranscriptChunk)
			if err := proto.Unmarshal(payload, chunk); err != nil {
				t.Fatal(err)
			}
			chunks = append(chunks, chunk)
		}
		if len(chunks) < 2 || chunks[0].GetHeader().GetSandboxSettings().GetSelected() != "FRESH" || !chunks[len(chunks)-1].GetDone() {
			t.Fatalf("expected transcript header and completion marker, got %v", chunks)
		}
		if tc.owner == "other" {
			if len(chunks) != 2 || chunks[0].GetHeader().GetSession() != nil {
				t.Fatalf("transcript disclosed another account's session: %v", chunks)
			}
			continue
		}
		if tc.first != 0 {
			if len(chunks) != 3 || chunks[0].GetHeader().GetNextBeforeSequence() != tc.next ||
				chunks[0].GetHeader().GetSession().GetTurnCount() != 10 {
				t.Fatalf("unexpected paged transcript header: %v", chunks[0])
			}
			turns := chunks[1].GetTurns().GetTurns()
			if len(turns) != int(tc.last-tc.first+1) || turns[0].Sequence != tc.first || turns[len(turns)-1].Sequence != tc.last {
				t.Fatalf("unexpected paged transcript turns: %v", turns)
			}
			continue
		}
		if len(chunks) != 4 || chunks[0].GetHeader().GetSession().GetTurnCount() != 10 {
			t.Fatalf("expected header and two turn batches, got %v", chunks)
		}
		wantOptions := &sessionpb.RunOptions{Model: "chosen-model", Permission: run.Workspace, Thinking: "high", McpServers: []string{"saved-mcp"}, ArrowJs: true}
		if options := chunks[0].GetHeader().GetLastOptions(); !proto.Equal(options, wantOptions) {
			t.Fatalf("unexpected composer options: %v", options)
		}
		first, last := chunks[1].GetTurns().GetTurns(), chunks[2].GetTurns().GetTurns()
		if len(first) != 8 || len(last) != 2 || last[1].GetSequence() != 10 || first[0].GetAttachments()[0] != "input.png" {
			t.Fatal("unexpected streamed turn batches")
		}
		parts := first[0].GetParts()
		if len(parts) != 1 || parts[0].GetKind() != "ui" || parts[0].GetText() != "Total: 42" || parts[0].GetOutput() != fixtureUISpec {
			t.Fatalf("unexpected rich display part: %v", parts)
		}
		want := &sessionpb.TokenUsage{Input: 3_100, Cached: 2_122_000, Output: 30_210}
		if !proto.Equal(first[0].GetTokenUsage(), want) || first[1].GetTokenUsage() != nil {
			t.Fatalf("expected token usage only for the measured turn: %v, %v", first[0].GetTokenUsage(), first[1].GetTokenUsage())
		}
	}
}

func TestTranscriptQueuesAreIndependentAndCancelable(t *testing.T) {
	const paged, full = `{"id":"owner"}`, `{"id":"owner","paged":false}`
	for _, tc := range []struct {
		name, queuedBody, readyBody string
		blockFull                   bool
	}{
		{name: "full", queuedBody: full, readyBody: paged, blockFull: true},
		{name: "paged", queuedBody: paged, readyBody: full},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				handler := fixtureHandler(t)
				handler.fullTranscriptSlots = semaphore.NewWeighted(1)
				handler.pagedTranscriptSlots = semaphore.NewWeighted(1)
				slots := handler.pagedTranscriptSlots
				if tc.blockFull {
					slots = handler.fullTranscriptSlots
				}
				if !slots.TryAcquire(1) {
					t.Fatal("reserve the transcript slot")
				}

				owner := auth.WithUser(t.Context(), auth.User{ID: "owner"})
				queued, cancel := context.WithCancel(owner)
				defer cancel()
				request := func(ctx context.Context, body string) *http.Request {
					return httptest.NewRequestWithContext(ctx, http.MethodPost, TranscriptPath, strings.NewReader(body))
				}
				waiting := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					defer close(done)
					handler.Transcript(waiting, request(queued, tc.queuedBody))
				}()
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("transcript did not wait for a slot")
				default:
				}

				ready := httptest.NewRecorder()
				handler.Transcript(ready, request(owner, tc.readyBody))
				if !ready.Flushed || ready.Header().Get("Content-Type") != transcriptType {
					t.Fatal("transcript did not stream while the other queue was busy")
				}

				cancel()
				<-done
				if waiting.Body.Len() != 0 {
					t.Fatal("canceled transcript wrote a response")
				}
				slots.Release(1)
				if !slots.TryAcquire(1) {
					t.Fatal("transcript slot was lost after cancellation")
				}
				slots.Release(1)
			})
		})
	}
}

func TestTranscriptPublishesServerTiming(t *testing.T) {
	handler := fixtureHandler(t)
	server := httptest.NewServer(httpserver.ServerTiming(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.Transcript(w, r.WithContext(auth.WithUser(r.Context(), auth.User{ID: "owner"})))
	})))
	defer server.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+TranscriptPath,
		strings.NewReader(`{"id":"owner","paged":true}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected transcript status: %d", response.StatusCode)
	}
	for _, phase := range []string{"app", "transcript_decode", "transcript_wait", "transcript_load", "sandbox_settings", "transcript_first_frame"} {
		if !strings.Contains(strings.Join(response.Header.Values("Server-Timing"), ","), phase+";dur=") {
			t.Errorf("transcript response is missing %s", phase)
		}
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"transcript_stream", "transcript_build_chunks", "protobuf_encode", "zstd_compress", "stream_write_flush"} {
		if !strings.Contains(strings.Join(response.Trailer.Values("Server-Timing"), ","), phase+";dur=") {
			t.Errorf("transcript trailer is missing %s", phase)
		}
	}
}
