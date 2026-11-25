package sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

func BenchmarkTranscript(b *testing.B) {
	for _, fixture := range []struct {
		name       string
		turns      int
		toolParts  int
		imageEvery int
	}{
		{name: "text", turns: 12, toolParts: 1},
		{name: "tool-heavy", turns: 12, toolParts: 8},
		{name: "with-images", turns: 12, toolParts: 8, imageEvery: 6},
		{name: "long", turns: 120, toolParts: 4},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			ctx := b.Context()
			repo, err := Open(ctx, filepath.Join(b.TempDir(), "transcript.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = repo.Close() })
			const owner, session = "benchmark-owner", "benchmark-session"
			now := time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)
			if err := repo.CreateSession(ctx, run.Session{ID: session, OwnerID: owner, CreatedAt: now, UpdatedAt: now}); err != nil {
				b.Fatal(err)
			}
			toolText := strings.Repeat("file.go:42: inspected the change and verified the result\n", 75)
			imageData := ""
			if fixture.imageEvery > 0 {
				imageData = strings.Repeat("AAAA", 1<<19) // 1.5 MiB after base64 decoding.
			}
			for i := range fixture.turns {
				id := fmt.Sprint(i)
				payload := run.Payload{Options: run.Options{Model: "benchmark", Permission: run.Workspace}}
				if fixture.imageEvery > 0 && i%fixture.imageEvery == 0 {
					payload.Images = []run.Image{{Name: "capture.png", MediaType: "image/png", Data: imageData}}
				}
				if _, err := repo.Enqueue(ctx, run.Run{
					ID: id, OwnerID: owner, SessionID: session, Status: run.Queued,
					Prompt: "Review the change and verify the results.", Policy: sandbox.Shared,
					Payload: payload, CreatedAt: now, UpdatedAt: now,
				}, "Benchmark transcript", false); err != nil {
					b.Fatal(err)
				}
				if claimed, err := repo.ClaimNext(ctx, now); err != nil || claimed == nil || claimed.ID != id {
					b.Fatalf("claim turn %s: %v", id, err)
				}
				parts := []run.TurnPart{{Kind: "text", Text: "Checking the result."}}
				for range fixture.toolParts {
					parts = append(parts, run.TurnPart{
						Kind: "tool", Tool: "bash", Input: `{"command":"go test ./..."}`,
						Output: toolText, Status: string(run.Succeeded),
					})
				}
				if err := repo.Complete(ctx, id, run.Completion{
					Status: run.Succeeded, UpdatedAt: now,
					Result: run.Result{Output: "Verified the change.", Parts: parts, AgentMessages: []byte(strings.Repeat(toolText, fixture.toolParts))},
				}); err != nil {
					b.Fatal(err)
				}
			}
			transcript, err := repo.Transcript(ctx, owner, session, 0)
			if err != nil || transcript == nil || len(transcript.Turns) != fixture.turns {
				b.Fatalf("load fixture transcript: %v", err)
			}
			if fixture.imageEvery > 0 {
				images := transcript.Turns[0].Payload.Images
				if len(images) != 1 || images[0].Name != "capture.png" || images[0].MediaType != "image/png" || images[0].Data != "" {
					b.Fatal("the transcript must contain image metadata only")
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := repo.Transcript(ctx, owner, session, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
