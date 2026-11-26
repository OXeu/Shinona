package pigo

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/coding"

	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

const (
	storageOwner   = "benchmark-owner"
	storageSession = "benchmark-session"
	summaryText    = "Summary of the earlier completed turns."
)

var storageModel = ai.Model{Api: ai.APIOpenAICompletions, Provider: "openai", ID: "benchmark"}

func BenchmarkSessionStorageAppend(b *testing.B) {
	for _, shape := range []struct {
		name  string
		tools int
	}{{"text", 0}, {"tools", 4}} {
		turn := makeStorageTurn(shape.tools)
		b.Run(shape.name, func(b *testing.B) {
			for _, backend := range []string{"SQLiteApp", "SQLitePayload", "PiJSONL"} {
				b.Run(backend, func(b *testing.B) {
					var write func(int)
					var jsonlPath string
					switch backend {
					case "SQLiteApp":
						repo := openStorageRepo(b)
						write = func(sequence int) { appendStorageTurn(b, repo, sequence, turn, nil) }
					case "SQLitePayload":
						write = openPayloadStore(b, turn)
					case "PiJSONL":
						recorder := openStorageJSONL(b)
						jsonlPath = recorder.Path()
						write = func(int) { appendJSONLTurn(b, recorder, turn.messages) }
					}
					b.SetBytes(turn.bytes)
					b.ReportAllocs()
					for sequence := 1; b.Loop(); sequence++ {
						write(sequence)
					}
					if jsonlPath != "" {
						info, err := os.Stat(jsonlPath)
						if err != nil || info.Size() < int64(b.N)*turn.bytes {
							b.Fatalf("JSONL file did not grow with the recorded turns: %v", err)
						}
					}
					b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "turns/s")
				})
			}
		})
	}
}

func BenchmarkSessionStorageRestore(b *testing.B) {
	for _, shape := range []struct {
		name   string
		turns  int
		retain int
	}{
		{"12Turns", 12, 0},
		{"120Turns", 120, 0},
		{"1200Turns", 1200, 0},
		{"1200TurnsCompacted", 1200, 30},
	} {
		b.Run(shape.name, func(b *testing.B) {
			turn := makeStorageTurn(4)
			wantMessages := shape.turns * len(turn.messages)
			if shape.retain > 0 {
				wantMessages = 1 + shape.retain*len(turn.messages)
			}
			for _, backend := range []string{"SQLiteApp", "PiJSONL"} {
				b.Run(backend, func(b *testing.B) {
					var load func() ([]agent.AgentMessage, error)
					if backend == "SQLiteApp" {
						repo := openStorageRepo(b)
						for sequence := 1; sequence <= shape.turns; sequence++ {
							var compacted []byte
							if shape.retain > 0 && sequence == shape.turns {
								messages := []agent.AgentMessage{ai.NewUserText(summaryText, 0)}
								for range shape.retain {
									messages = append(messages, turn.messages...)
								}
								var err error
								compacted, err = encodeMessages(messages, true)
								if err != nil {
									b.Fatal(err)
								}
							}
							appendStorageTurn(b, repo, sequence, turn, compacted)
						}
						load = func() ([]agent.AgentMessage, error) {
							history, err := repo.History(b.Context(), run.Run{
								OwnerID: storageOwner, SessionID: storageSession, Sequence: shape.turns + 1,
							}, 0)
							if err != nil {
								return nil, err
							}
							return conversationHistory(history, storageModel)
						}
					} else {
						path := seedStorageJSONL(b, turn, shape.turns, shape.retain)
						load = func() ([]agent.AgentMessage, error) {
							tree, err := coding.LoadSessionTree(path)
							if err != nil {
								return nil, err
							}
							return tree.BuildProjection().Messages, nil
						}
					}
					b.ReportAllocs()
					for b.Loop() {
						messages, err := load()
						if err != nil || len(messages) != wantMessages {
							b.Fatalf("restore got %d messages, want %d: %v", len(messages), wantMessages, err)
						}
					}
					b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "restores/s")
				})
			}
		})
	}
}

type storageTurn struct {
	messages []agent.AgentMessage
	prompt   string
	answer   string
	parts    []run.TurnPart
	bytes    int64
}

func makeStorageTurn(tools int) storageTurn {
	prompt := "Review the sandbox changes, run the necessary checks, and summarize the results."
	answer := strings.Repeat("The checks passed. The updated command and configuration are ready for review. ", 9)
	assistant := func(reason ai.StopReason, content ai.ContentList) ai.AssistantMessage {
		return ai.AssistantMessage{Api: storageModel.Api, Provider: storageModel.Provider, Model: storageModel.ID,
			StopReason: reason, Content: content}
	}
	messages := []agent.AgentMessage{ai.NewUserText(prompt, 0)}
	for i := range tools {
		id := fmt.Sprintf("call_%d", i)
		var output strings.Builder
		for line := range 65 {
			fmt.Fprintf(&output, "package/%02d/main.go:%03d: verified test result %08x for workspace\n", i, line, (i+1)*(line+37)*51313)
		}
		messages = append(messages,
			assistant(ai.StopToolUse, ai.ContentList{ai.ToolCall{ID: id, Name: "bash", Arguments: map[string]any{"command": "go test ./pkg/..."}}}),
			ai.ToolResultMessage{ToolCallID: id, ToolName: "bash", Content: ai.ContentList{ai.TextContent{Text: output.String()}}},
		)
	}
	messages = append(messages, assistant(ai.StopStop, ai.ContentList{ai.TextContent{Text: answer}}))
	encoded, _ := json.Marshal(messages)
	return storageTurn{messages: messages, prompt: prompt, answer: answer, parts: transcriptParts(messages, true), bytes: int64(len(encoded))}
}

func openStorageRepo(b *testing.B) *sqlite.Repository {
	b.Helper()
	repo, err := sqlite.Open(b.Context(), filepath.Join(b.TempDir(), "session.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = repo.Close() })
	if err := repo.CreateSession(b.Context(), run.Session{ID: storageSession, OwnerID: storageOwner}); err != nil {
		b.Fatal(err)
	}
	return repo
}

func appendStorageTurn(b *testing.B, repo *sqlite.Repository, sequence int, turn storageTurn, compacted []byte) {
	b.Helper()
	ctx := b.Context()
	now := time.Now().UTC()
	id := strconv.Itoa(sequence)
	if _, err := repo.Enqueue(ctx, run.Run{
		ID: id, OwnerID: storageOwner, SessionID: storageSession, Status: run.Queued,
		Prompt: turn.prompt, Policy: sandbox.Shared, CreatedAt: now, UpdatedAt: now,
	}, "Benchmark session", false); err != nil {
		b.Fatal(err)
	}
	if claimed, err := repo.ClaimNext(ctx, now); err != nil || claimed == nil || claimed.ID != id {
		b.Fatalf("claim turn %s: %v", id, err)
	}
	messages, err := encodeMessages(turn.messages, false)
	if err != nil {
		b.Fatal(err)
	}
	if err := repo.Complete(ctx, id, run.Completion{
		Status: run.Succeeded, UpdatedAt: now,
		Result: run.Result{Output: turn.answer, Parts: turn.parts, AgentMessages: messages, CompactedContext: compacted},
	}); err != nil {
		b.Fatal(err)
	}
}

func openPayloadStore(b *testing.B, turn storageTurn) func(int) {
	b.Helper()
	ctx := b.Context()
	db, err := sql.Open("sqlite3", filepath.Join(b.TempDir(), "payload.db")+"?_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		b.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	b.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `CREATE TABLE turns (sequence INTEGER PRIMARY KEY, messages BLOB NOT NULL)`); err != nil {
		b.Fatal(err)
	}
	insert, err := db.PrepareContext(ctx, `INSERT INTO turns(sequence, messages) VALUES (?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = insert.Close() })
	return func(sequence int) {
		data, err := json.Marshal(turn.messages)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := insert.ExecContext(ctx, sequence, data); err != nil {
			b.Fatal(err)
		}
	}
}

func openStorageJSONL(b *testing.B) *coding.SessionRecorder {
	b.Helper()
	path := filepath.Join(b.TempDir(), "session.jsonl")
	header := fmt.Sprintf("{\"type\":\"session\",\"version\":%d,\"id\":\"bench\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"cwd\":\"/workspace\"}\n", coding.CurrentSessionVersion)
	if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
		b.Fatal(err)
	}
	recorder, err := coding.ResumeSession(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = recorder.Close() })
	return recorder
}

func appendJSONLTurn(b *testing.B, recorder *coding.SessionRecorder, messages []agent.AgentMessage) string {
	b.Helper()
	first := recorder.RecordMessage(messages[0])
	if first == "" {
		b.Fatal("Pi did not record the user message")
	}
	for _, message := range messages[1:] {
		if recorder.RecordMessage(message) == "" {
			b.Fatal("Pi did not record the message")
		}
	}
	return first
}

func seedStorageJSONL(b *testing.B, turn storageTurn, turns, retain int) string {
	b.Helper()
	recorder := openStorageJSONL(b)
	var firstKept string
	for sequence := 1; sequence <= turns; sequence++ {
		first := appendJSONLTurn(b, recorder, turn.messages)
		if sequence == turns-retain+1 {
			firstKept = first
		}
	}
	if retain > 0 {
		// SessionRecorder exposes message appends. Seed the native compaction entry
		// directly so both stores restore the same number of messages.
		file, err := os.OpenFile(recorder.Path(), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			b.Fatal(err)
		}
		err = json.NewEncoder(file).Encode(map[string]any{
			"type": "compaction", "id": "bench-compaction", "parentId": recorder.LastEntryID(),
			"timestamp": "2026-01-01T00:00:01Z", "summary": summaryText, "firstKeptEntryId": firstKept,
		})
		if closeErr := file.Close(); closeErr != nil {
			b.Fatal(closeErr)
		}
		if err != nil {
			b.Fatal(err)
		}
	}
	return recorder.Path()
}
