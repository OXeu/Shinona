package sessiontransfer_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

type stagedAgent struct {
	steps chan func(run.Input)
	done  chan struct{}
}

type timingLogs chan string

func (logs timingLogs) Write(data []byte) (int, error) {
	logs <- string(data)
	return len(data), nil
}

func (a stagedAgent) Run(ctx context.Context, input run.Input, _ sandbox.Workspace) (run.Result, error) {
	input.ReportParts([]run.TurnPart{{Kind: "text", Text: "initial text"}})
	for {
		select {
		case step := <-a.steps:
			step(input)
		case <-a.done:
			return run.Result{Output: "Final reply."}, nil
		case <-ctx.Done():
			return run.Result{}, ctx.Err()
		}
	}
}

func TestLiveSubscriptionStreamsDeltasAndReconcilesAfterReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	repo, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	const owner = "owner"
	logger := slog.New(slog.DiscardHandler)
	agent := stagedAgent{steps: make(chan func(run.Input), 4), done: make(chan struct{})}
	resolve := func(context.Context, string, string) (run.Target, error) {
		return run.Target{Name: "demo", Provider: demo.Provider{}, ResolveAgent: func(context.Context, string) (run.Agent, error) { return agent, nil }}, nil
	}
	controls := &run.Controls{}
	service := run.NewService(repo, resolve, uuid.NewString, controls, nil)
	entry, err := service.CreateSession(ctx, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const earlier = 4
	var known []*sessionpb.TurnVersion
	for sequence := 1; sequence <= run.TranscriptPageSize+earlier; sequence++ {
		id := fmt.Sprintf("earlier-%d", sequence)
		if _, err := repo.Enqueue(ctx, run.Run{
			ID: id, OwnerID: owner, SessionID: entry.ID, Policy: sandbox.Fresh, Prompt: "Prior turn",
			Status: run.Queued, CreatedAt: now, UpdatedAt: now,
		}, "Prior", false); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.ClaimNext(ctx, now); err != nil {
			t.Fatal(err)
		}
		if err := repo.Complete(ctx, id, run.Completion{
			Result: run.Result{Output: "Prior reply."}, Status: run.Succeeded, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if sequence > earlier {
			known = append(known, &sessionpb.TurnVersion{Id: id, UpdatedAt: now.Format(time.RFC3339Nano), Status: string(run.Succeeded)})
		}
	}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		run.NewWorker(repo, resolve, logger, controls).Serve(ctx)
	}()
	defer func() {
		cancel()
		<-workerDone
	}()

	logs := make(timingLogs, 16)
	handler, err := sessiontransfer.New(service, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	var revoked atomic.Bool
	const origin = "https://shinona.example"
	live := handler.Live(ctx, origin, func(r *http.Request) (*auth.User, error) {
		if r.Header.Get("X-Test-Cookie") == "revocable" && revoked.Load() {
			return nil, nil
		}
		return &auth.User{ID: r.Header.Get("X-Test-Owner")}, nil
	}, service)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := auth.User{ID: r.Header.Get("X-Test-Owner")}
		live.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
	}))
	defer server.Close()
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()

	dial := func(account, token string) *websocket.Conn {
		t.Helper()
		header := http.Header{"Origin": {origin}, "X-Test-Owner": {account}, "X-Test-Cookie": {token}}
		url := "ws" + strings.TrimPrefix(server.URL, "http")
		conn, response, err := websocket.DefaultDialer.Dial(url, header)
		if response != nil {
			defer func() { _ = response.Body.Close() }()
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		payload, err := proto.Marshal(&sessionpb.LiveRequest{SessionId: entry.ID, KnownTurns: known})
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	waitFor := func(conn *websocket.Conn, seq *uint32, match func(*sessionpb.LiveMessage) bool) *sessionpb.LiveMessage {
		t.Helper()
		for {
			kind, raw, err := conn.ReadMessage()
			if err != nil || kind != websocket.BinaryMessage {
				t.Fatalf("read live protobuf: %v (kind %d)", err, kind)
			}
			data, err := decoder.DecodeAll(raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			var message sessionpb.LiveMessage
			if err := proto.Unmarshal(data, &message); err != nil {
				t.Fatal(err)
			}
			*seq++
			if message.Sequence != *seq {
				t.Fatalf("got sequence %d, want %d", message.Sequence, *seq)
			}
			if match(&message) {
				return &message
			}
		}
	}

	foreign := dial("other", "")
	if _, _, err := foreign.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("cross-account subscription: %v", err)
	}
	conn := dial(owner, "")
	var seq uint32
	start := waitFor(conn, &seq, func(msg *sessionpb.LiveMessage) bool { return msg.GetSync() != nil }).GetSync()
	if len(start.TurnIds) != run.TranscriptPageSize || len(start.ChangedTurns) != 0 || start.NextBeforeSequence != earlier+1 {
		t.Fatalf("unexpected paged initial reconciliation: %v", start)
	}
	current, err := service.Submit(ctx, owner, entry.ID, "Now stream.", run.Payload{}, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(conn, &seq, func(msg *sessionpb.LiveMessage) bool {
		for _, turn := range msg.GetSync().GetChangedTurns() {
			if turn.Id == current.ID && len(turn.Parts) > 0 {
				return true
			}
		}
		for _, part := range msg.GetProgress().GetParts() {
			if part.GetReplace().GetText() == "initial text" {
				return true
			}
		}
		return false
	})
	agent.steps <- func(input run.Input) {
		input.ReportParts([]run.TurnPart{{Kind: "text", Text: "initial text and delta"}})
	}
	progress := waitFor(conn, &seq, func(msg *sessionpb.LiveMessage) bool { return len(msg.GetProgress().GetParts()) != 0 }).GetProgress()
	if progress.TurnId != current.ID || len(progress.Parts) != 1 || progress.Parts[0].GetAppend().GetText() != " and delta" {
		t.Fatalf("expected suffix-only progress: %v", progress)
	}
	_ = conn.Close()
	waitForBootstrapAndDeltaTiming(t, ctx, logs)

	resumed := dial(owner, "")
	var resumedSeq uint32
	resume := waitFor(resumed, &resumedSeq, func(msg *sessionpb.LiveMessage) bool { return msg.GetSync() != nil }).GetSync()
	if len(resume.ChangedTurns) != 1 || resume.ChangedTurns[0].GetParts()[0].Text != "initial text and delta" {
		t.Fatalf("reconnect did not reconcile current progress: %v", resume)
	}
	revocable := dial(owner, "revocable")
	var revocableSeq uint32
	waitFor(revocable, &revocableSeq, func(msg *sessionpb.LiveMessage) bool { return msg.GetSync() != nil })
	revoked.Store(true)
	if err := service.Steer(owner, entry.ID, current.ID, "Steer this turn."); err != nil {
		t.Fatal(err)
	}
	if _, _, err := revocable.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("revoked subscription stayed open: %v", err)
	}
	waitFor(resumed, &resumedSeq, func(msg *sessionpb.LiveMessage) bool {
		messages := msg.GetProgress().GetSteering().GetMessages()
		return len(messages) == 1 && !messages[0].Delivered
	})
	agent.steps <- func(input run.Input) { _ = input.Steering.Drain() }
	waitFor(resumed, &resumedSeq, func(msg *sessionpb.LiveMessage) bool {
		messages := msg.GetProgress().GetSteering().GetMessages()
		return len(messages) == 1 && messages[0].Delivered
	})
	close(agent.done)
	final := waitFor(resumed, &resumedSeq, func(msg *sessionpb.LiveMessage) bool {
		return msg.GetSync().GetSession() != nil && !msg.GetSync().GetSession().Active
	}).GetSync()
	if len(final.ChangedTurns) != 1 || final.ChangedTurns[0].Output != "Final reply." ||
		len(final.TurnIds) != run.TranscriptPageSize || final.NextBeforeSequence != earlier+2 {
		t.Fatalf("unexpected terminal reconciliation: %v", final)
	}
}

func waitForBootstrapAndDeltaTiming(t *testing.T, ctx context.Context, logs timingLogs) {
	t.Helper()
	for {
		select {
		case entry := <-logs:
			if strings.Contains(entry, "phases.live_send_delta.ms=") {
				for _, phase := range []string{"live_bootstrap", "live_load_transcript", "live_build_delta", "protobuf_encode", "zstd_compress", "websocket_write"} {
					if !strings.Contains(entry, "phases."+phase+".ms=") {
						t.Errorf("live timing report is missing %s", phase)
					}
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("missing live timing report after a bootstrap and delta")
		}
	}
}
