//go:build linux

package sqlite

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/graphql"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sqlite/transferpb"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

type replayTurn struct {
	Prompt      string
	Output      string
	Attachments []string
	Parts       []run.TurnPart
}

type replayFixture struct {
	SourceBytes int
	SourcePairs int
	Turns       []replayTurn
}

type replayState struct {
	parts  []run.TurnPart
	cursor int
}

type replayFeed struct {
	mu        sync.Mutex
	state     replayState
	initial   []run.TurnPart
	steps     []*transferpb.LiveDelta
	published []int64
	changed   chan struct{}
	running   bool
}

func newReplayFeed(parts []run.TurnPart) *replayFeed {
	feed := &replayFeed{changed: make(chan struct{})}
	index := 0
	for _, part := range parts {
		content := part.Text
		if part.Kind == "tool" {
			content = part.Output
		}
		if len(content) < 12 {
			continue
		}
		// Sanitized text is ASCII, so chunks keep valid UTF-8 at every update.
		size := (len(content) + 7) / 8
		metadata := &transferpb.TurnPart{Kind: part.Kind, Tool: part.Tool, Input: part.Input, Status: part.Status}
		if index == 0 {
			part.Text, part.Output = "", ""
			appendReplayText(&part, content[:size])
			feed.initial = []run.TurnPart{part}
			content, metadata = content[size:], nil
		}
		for start := 0; start < len(content); start += size {
			feed.steps = append(feed.steps, &transferpb.LiveDelta{
				Cursor: uint32(len(feed.steps) + 1), PartIndex: uint32(index),
				Part: metadata, AppendText: content[start:min(start+size, len(content))],
			})
			metadata = nil
		}
		index++
		if index == 6 {
			break
		}
	}
	feed.state.parts = slices.Clone(feed.initial)
	return feed
}

func appendReplayText(part *run.TurnPart, text string) {
	if part.Kind == "tool" {
		part.Output += text
	} else {
		part.Text += text
	}
}

func (f *replayFeed) snapshot() replayState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return replayState{parts: slices.Clone(f.state.parts), cursor: f.state.cursor}
}

func (f *replayFeed) reset() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return false
	}
	f.state = replayState{parts: slices.Clone(f.initial)}
	f.published = nil
	return true
}

func (f *replayFeed) start(ctx context.Context, interval time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running || f.state.cursor != 0 {
		return false
	}
	f.running = true
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for _, step := range f.steps {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			f.mu.Lock()
			if step.Part != nil {
				f.state.parts = append(f.state.parts, run.TurnPart{
					Kind: step.Part.Kind, Tool: step.Part.Tool, Input: step.Part.Input, Status: step.Part.Status,
				})
			}
			appendReplayText(&f.state.parts[step.PartIndex], step.AppendText)
			f.state.cursor++
			f.published = append(f.published, time.Now().UnixMicro())
			if f.state.cursor == len(f.steps) {
				f.running = false
			}
			close(f.changed)
			f.changed = make(chan struct{})
			f.mu.Unlock()
		}
	}()
	return true
}

func (f *replayFeed) stream(ctx context.Context, send func(*transferpb.LiveDelta) error) error {
	for next := 0; next < len(f.steps); {
		f.mu.Lock()
		current, changed := f.state.cursor, f.changed
		f.mu.Unlock()
		for ; next < current; next++ {
			if err := send(f.steps[next]); err != nil {
				return err
			}
		}
		if next < len(f.steps) {
			select {
			case <-changed:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

type replaySnapshotKey struct{}

type replayReader struct{ *Repository }

func (r replayReader) Transcript(ctx context.Context, owner, id string, limit int) (*run.Transcript, error) {
	page, err := r.Repository.Transcript(ctx, owner, id, limit)
	if err == nil && page != nil {
		if snapshot, ok := ctx.Value(replaySnapshotKey{}).(replayState); ok {
			page.Turns[len(page.Turns)-1].Parts = snapshot.parts
		}
	}
	return page, err
}

func (fixture replayFixture) seed(t *testing.T, repo *Repository) {
	t.Helper()
	epoch := time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)
	count := len(fixture.Turns)
	session := sessionRow{
		ID: transferSession, OwnerID: transferOwner, Title: "Sanitized session", TurnCount: count,
		ReadThrough: count - 1, CreatedAt: epoch, UpdatedAt: epoch.Add(time.Duration(count) * time.Minute),
	}
	db := repo.db.WithContext(t.Context())
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	rows := make([]row, 0, count)
	for i, turn := range fixture.Turns {
		at := epoch.Add(time.Duration(i) * time.Minute)
		entry := row{
			ID: fmt.Sprintf("turn-%d", i+1), OwnerID: transferOwner, SessionID: transferSession,
			Sequence: i + 1, Policy: sandbox.Fresh, Prompt: turn.Prompt, Status: run.Succeeded,
			Output: turn.Output, Parts: turn.Parts, CreatedAt: at, StartedAt: &at, UpdatedAt: at,
			Payload: run.Payload{Options: run.Options{Model: "fixture-model"}},
		}
		for _, name := range turn.Attachments {
			entry.Payload.Images = append(entry.Payload.Images, run.Image{Name: name})
		}
		if i == count-1 {
			entry.Status, entry.Parts = run.Running, nil
		}
		rows = append(rows, entry)
	}
	if err := db.CreateInBatches(rows, 10).Error; err != nil {
		t.Fatal(err)
	}
}

func TestRealtimeTransferBenchmarkServer(t *testing.T) {
	path := os.Getenv("SHINONA_REALTIME_FIXTURE")
	if path == "" {
		t.Skip("started by the realtime transfer benchmark runner")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read sanitized fixture")
	}
	var fixture replayFixture
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Turns) < 2 || len(fixture.Turns) > 50 {
		t.Fatal("invalid sanitized fixture")
	}
	intervalMS, err := strconv.Atoi(os.Getenv("SHINONA_REALTIME_INTERVAL_MS"))
	if err != nil || intervalMS < 20 || intervalMS > 1000 {
		t.Fatal("invalid realtime interval")
	}
	interval := time.Duration(intervalMS) * time.Millisecond
	feed := newReplayFeed(fixture.Turns[len(fixture.Turns)-1].Parts)
	if len(feed.steps) < 30 {
		t.Fatal("active turn has insufficient append data")
	}
	repo, err := Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	fixture.seed(t, repo)

	target := func(string) run.Target {
		return run.Target{Name: "demo", Provider: demo.Provider{}, ResolveAgent: func(context.Context, string) (run.Agent, error) { return demo.Agent{}, nil }}
	}
	service := run.NewService(replayReader{repo}, target, nil, &run.Controls{})
	logger := slog.New(slog.DiscardHandler)
	graphqlHandler, err := graphql.NewHandler(service, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := sessiontransfer.New(service, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	zstdEncoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zstdEncoder.Close() })

	mux := http.NewServeMux()
	bindSnapshot := func(handler http.Handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			exact := feed.snapshot()
			w.Header().Set("X-Bench-Cursor", strconv.Itoa(exact.cursor))
			ctx := context.WithValue(r.Context(), replaySnapshotKey{}, exact)
			handler.ServeHTTP(w, r.WithContext(auth.WithUser(ctx, auth.User{ID: transferOwner})))
		}
	}
	mux.HandleFunc("POST /graphql", bindSnapshot(graphqlHandler))
	mux.HandleFunc("POST "+sessiontransfer.TranscriptPath, bindSnapshot(http.HandlerFunc(transcript.Transcript)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>Shinona active session benchmark</title>")
	})
	mux.Handle("GET /bench/", http.StripPrefix("/bench/", http.FileServer(http.Dir(os.Getenv("SHINONA_TRANSFER_ASSETS")))))
	mux.HandleFunc("GET /measure", transferMeasure)
	mux.HandleFunc("GET /live/fixture", func(w http.ResponseWriter, _ *http.Request) {
		writeTransferJSON(w, map[string]int{
			"sourceBytes": fixture.SourceBytes, "sourcePairs": fixture.SourcePairs, "turns": len(fixture.Turns),
			"updates": len(feed.steps), "intervalMs": intervalMS,
		})
	})
	mux.HandleFunc("POST /live/reset", func(w http.ResponseWriter, _ *http.Request) {
		if !feed.reset() {
			http.Error(w, "replay is active", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	lifetime, stop := context.WithCancel(t.Context())
	defer stop()
	mux.HandleFunc("POST /live/start", func(w http.ResponseWriter, _ *http.Request) {
		if !feed.start(lifetime, interval) {
			http.Error(w, "reset the replay before starting", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /live/timing", func(w http.ResponseWriter, _ *http.Request) {
		feed.mu.Lock()
		times := slices.Clone(feed.published)
		feed.mu.Unlock()
		writeTransferJSON(w, times)
	})
	mux.HandleFunc("GET /live/poll", feed.servePoll)
	mux.HandleFunc("GET /live/stream/{codec}", feed.serveStream(zstdEncoder))
	mux.HandleFunc("GET /live/ws/{codec}", feed.serveSocket(zstdEncoder))
	server := httptest.NewServer(mux)
	defer server.Close()
	fmt.Printf("SHINONA_TRANSFER_URL=%s\n", server.URL)
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Error(err)
	}
}

func (f *replayFeed) servePoll(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.Atoi(r.URL.Query().Get("after"))
	current := f.snapshot().cursor
	if err != nil || after < 0 || after > current {
		http.Error(w, "invalid cursor", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	for _, step := range f.steps[after:current] {
		payload, err := protojson.Marshal(step)
		if err != nil {
			return
		}
		if _, err := w.Write(append(payload, '\n')); err != nil {
			return
		}
	}
}

func replayEncoder(codec string, zstdEncoder *zstd.Encoder) (func(*transferpb.LiveDelta) ([]byte, error), error) {
	switch codec {
	case "ndjson":
		return func(step *transferpb.LiveDelta) ([]byte, error) {
			data, err := protojson.Marshal(step)
			return append(data, '\n'), err
		}, nil
	case "protobuf":
		return func(step *transferpb.LiveDelta) ([]byte, error) { return proto.Marshal(step) }, nil
	case "protobuf-zstd":
		return func(step *transferpb.LiveDelta) ([]byte, error) {
			data, err := proto.Marshal(step)
			if err != nil {
				return nil, err
			}
			return zstdEncoder.EncodeAll(data, nil), nil
		}, nil
	default:
		return nil, errors.New("invalid delta codec")
	}
}

func (f *replayFeed) serveStream(zstdEncoder *zstd.Encoder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		codec := r.PathValue("codec")
		encode, err := replayEncoder(codec, zstdEncoder)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if codec == "ndjson" {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		controller := http.NewResponseController(w)
		if err := controller.Flush(); err != nil {
			return
		}
		_ = f.stream(r.Context(), func(step *transferpb.LiveDelta) error {
			data, err := encode(step)
			if err != nil {
				return err
			}
			if codec == "ndjson" {
				if _, err := fmt.Fprintf(w, "data: %s\n\n", strings.TrimSuffix(string(data), "\n")); err != nil {
					return err
				}
			} else {
				header := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
				if _, err := w.Write(header); err != nil {
					return err
				}
				if _, err := w.Write(data); err != nil {
					return err
				}
			}
			return controller.Flush()
		})
	}
}

func (f *replayFeed) serveSocket(zstdEncoder *zstd.Encoder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		codec := r.PathValue("codec")
		encode, err := replayEncoder(codec, zstdEncoder)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		upgrader := websocket.Upgrader{HandshakeTimeout: 5 * time.Second}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		messageType := websocket.BinaryMessage
		if codec == "ndjson" {
			messageType = websocket.TextMessage
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		_ = f.stream(ctx, func(step *transferpb.LiveDelta) error {
			data, err := encode(step)
			if err != nil {
				return err
			}
			if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return err
			}
			return conn.WriteMessage(messageType, data)
		})
	}
}
