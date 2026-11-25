package sessiontransfer_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
)

func TestAttachmentServesOnlyTheOwnersVisibleTurn(t *testing.T) {
	ctx := t.Context()
	repo, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC()
	if err := repo.CreateSession(ctx, run.Session{ID: "session", OwnerID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	var pngFile bytes.Buffer
	if err := png.Encode(&pngFile, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	attachment := run.Image{Name: "fixture.png", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(pngFile.Bytes())}
	_, err = repo.Enqueue(ctx, run.Run{
		ID: "turn", OwnerID: "owner", SessionID: "session", Status: run.Queued, CreatedAt: now, UpdatedAt: now,
		Payload: run.Payload{Images: []run.Image{attachment}},
	}, "Fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.ClaimNext(ctx, now); err != nil || claimed == nil || claimed.ID != "turn" {
		t.Fatalf("claim fixture turn: %v, %v", claimed, err)
	}
	if err := repo.Complete(ctx, "turn", run.Completion{Status: run.Succeeded, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	service := run.NewService(repo, nil, nil, &run.Controls{})
	mux := http.NewServeMux()
	mux.Handle("GET "+sessiontransfer.AttachmentPath, sessiontransfer.Attachment(service, slog.New(slog.DiscardHandler)))
	requestImage := func(owner, session, index string) *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(auth.WithUser(ctx, auth.User{ID: owner}), http.MethodGet,
			sessiontransfer.TranscriptPath+"/"+session+"/turns/turn/attachments/"+index, nil)
		result := httptest.NewRecorder()
		mux.ServeHTTP(result, request)
		return result
	}
	result := requestImage("owner", "session", "0")
	if result.Code != http.StatusOK || result.Header().Get("Content-Type") != "image/png" ||
		result.Header().Get("Cache-Control") != "private, no-store" || !bytes.Equal(result.Body.Bytes(), pngFile.Bytes()) {
		t.Fatalf("attachment response: %d %v", result.Code, result.Header())
	}
	for _, request := range []struct{ owner, session, index string }{
		{"other", "session", "0"}, {"", "session", "0"}, {"owner", "missing", "0"},
		{"owner", "session", "1"}, {"owner", "session", "-1"},
	} {
		if result := requestImage(request.owner, request.session, request.index); result.Code != http.StatusNotFound {
			t.Fatalf("forbidden attachment request %v returned %d", request, result.Code)
		}
	}
	if _, err := repo.RevertTurn(ctx, "owner", "session", "turn", "turn", now); err != nil {
		t.Fatal(err)
	}
	if result := requestImage("owner", "session", "0"); result.Code != http.StatusNotFound {
		t.Fatalf("reverted image returned %d", result.Code)
	}
}
