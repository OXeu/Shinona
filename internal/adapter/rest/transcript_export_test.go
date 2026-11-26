package rest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/encoding/protodelim"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
)

func decodeTranscriptExport(t *testing.T, response *httptest.ResponseRecorder, failure sessionpb.TranscriptExportEnd_Failure) []*sessionpb.TranscriptExportFrame {
	t.Helper()
	if got := response.Header().Get("Content-Type"); got != sessiontransfer.TranscriptExportMediaType {
		t.Fatalf("export content type = %s", got)
	}
	body := response.Body.Bytes()
	if response.Header().Get("Content-Encoding") == "zstd" {
		decoder, err := zstd.NewReader(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer decoder.Close()
		body, err = decoder.DecodeAll(body, nil)
		if err != nil {
			t.Fatalf("decode compressed export: %v", err)
		}
	}
	reader := bufio.NewReader(bytes.NewReader(body))
	options := protodelim.UnmarshalOptions{MaxSize: maxFullTranscriptBytes}
	var frames []*sessionpb.TranscriptExportFrame
	for {
		frame := new(sessionpb.TranscriptExportFrame)
		if err := options.UnmarshalFrom(reader, frame); err != nil {
			t.Fatalf("export ended before its end frame: %v", err)
		}
		frames = append(frames, frame)
		if frame.GetEnd() != nil {
			if frame.GetEnd().GetFailure() != failure || reader.Buffered() != 0 {
				t.Fatalf("unexpected end frame: %s", frame.GetEnd())
			}
			if _, err := reader.Peek(1); err == nil {
				t.Fatal("export continued after its end frame")
			}
			break
		}
		if len(frames) == 1 {
			if frame.GetHeader().GetSandboxSettings() == nil {
				t.Fatal("the first frame should contain export settings")
			}
		} else if frame.GetTurn() == nil {
			t.Fatal("expected a turn after the header")
		}
	}
	if len(frames) < 2 {
		t.Fatal("export is missing its header")
	}
	return frames
}

func TestTranscriptExportReportsErrorsBeforeAndAfterResponseStarts(t *testing.T) {
	raw := make([]byte, 512<<10)
	random := rand.NewChaCha8([32]byte{})
	_, _ = random.Read(raw)
	turn := run.Run{ID: "first", Output: base64.StdEncoding.EncodeToString(raw)}
	const limit = 1 << 20
	for _, tt := range []struct {
		name    string
		zstd    bool
		limit   int
		nextErr error
		status  int
		code    string
		failure sessionpb.TranscriptExportEnd_Failure
	}{
		{name: "limit before start", zstd: true, limit: 1024, status: 413, code: "TRANSCRIPT_EXPORT_LIMIT"},
		{name: "limit after start", limit: limit, status: 200, failure: sessionpb.TranscriptExportEnd_LIMIT},
		{name: "limit after compressed start", zstd: true, limit: limit, status: 200, failure: sessionpb.TranscriptExportEnd_LIMIT},
		{name: "timeout after start", limit: limit, nextErr: context.DeadlineExceeded, status: 200, failure: sessionpb.TranscriptExportEnd_TIMEOUT},
		{name: "read failure after compressed start", zstd: true, limit: limit, nextErr: errors.New("private DB error"),
			status: 200, failure: sessionpb.TranscriptExportEnd_INTERNAL},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := &handler{logger: slog.New(slog.DiscardHandler)}
			stream := run.TranscriptStream{TurnCount: 2, Session: &run.Session{ID: "session"}}
			calls := 0
			stream.Next = func() ([]run.Run, error) {
				calls++
				if calls == 1 {
					return []run.Run{turn}, nil
				}
				return []run.Run{{ID: "second", Output: turn.Output}}, tt.nextErr
			}
			route := gin.New()
			route.GET("/", httpserver.Compress, func(c *gin.Context) {
				ctx, finish := httpserver.StreamTiming(c.Writer, c.Request.Context())
				defer finish()
				err := api.writeTranscriptExport(ctx, c, tt.zstd, nil, stream, run.Settings{}, tt.limit)
				if err == nil {
					t.Error("export should fail")
					return
				}
				api.writeTranscriptFailure(c, err)
			})
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			if tt.zstd {
				req.Header.Set("Accept-Encoding", "zstd")
			}
			recorder := serve(t, httpserver.ServerTiming(route), req, tt.status)
			if tt.code != "" {
				checkCode(t, recorder, tt.code)
				if recorder.Header().Get("Trailer") != "" || recorder.Header().Get("Content-Encoding") != "" {
					t.Fatalf("export headers leaked into the JSON error: %v", recorder.Header())
				}
				return
			}
			if tt.zstd && recorder.Header().Get("Content-Encoding") != "zstd" {
				t.Fatal("expected a compressed export")
			}
			frames := decodeTranscriptExport(t, recorder, tt.failure)
			if len(frames) != 3 || frames[1].GetTurn().GetId() != "first" {
				t.Fatal("the failed turn should not appear in the stream")
			}
		})
	}
}

func TestTranscriptExportDeadlineIncludesWaitingForSlot(t *testing.T) {
	repo, _ := restFixture(t)
	api := &handler{
		service: fixtureService(repo.RunStores()), logger: slog.New(slog.DiscardHandler), fullTranscript: semaphore.NewWeighted(1),
	}
	if !api.fullTranscript.TryAcquire(1) {
		t.Fatal("could not hold the full-transcript slot")
	}
	defer api.fullTranscript.Release(1)
	router := gin.New()
	router.GET(Prefix+"/sessions/:id/transcript", api.transcript)
	ctx, cancel := context.WithTimeout(auth.WithUser(t.Context(), auth.User{ID: "alice"}), 25*time.Millisecond)
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, Prefix+"/sessions/history/transcript?paged=false", nil)
	response := serve(t, router, request, http.StatusGatewayTimeout)
	checkCode(t, response, "TRANSCRIPT_EXPORT_TIMEOUT")
}
