// Package sessiontransfer serves signed-in session reads using binary Protobuf.
package sessiontransfer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
)

const (
	RecentPath     = "/api/session-transfer/v1/recent"
	TranscriptPath = "/api/session-transfer/v1/transcript"

	transcriptType = "application/vnd.shinona.transcript.v1+protobuf"
	turnBatchSize  = 8
)

type Reader interface {
	Projects(context.Context, string) ([]run.Project, error)
	RecentSessions(context.Context, string, *string) ([]run.Session, error)
	Transcript(context.Context, string, string) (*run.Transcript, error)
	Settings(context.Context, string) (run.Settings, error)
}

type Handler struct {
	reader Reader
	logger *slog.Logger
	brotli sync.Pool
	zstd   *zstd.Encoder
}

func New(reader Reader, logger *slog.Logger) (*Handler, error) {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return &Handler{
		reader: reader, logger: logger, zstd: encoder,
		brotli: sync.Pool{New: func() any { return brotli.NewWriterLevel(io.Discard, 4) }},
	}, nil
}

func (h *Handler) Close() error { return h.zstd.Close() }

// Recent requires an authenticated upgrade. It checks the original cookie again on each poll.
func (h *Handler) Recent(lifetime context.Context, origin string, sessionUser func(*http.Request) (*auth.User, error)) http.Handler {
	upgrader := websocket.Upgrader{
		HandshakeTimeout: 5 * time.Second,
		CheckOrigin:      func(r *http.Request) bool { return r.Header.Get("Origin") == origin },
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := auth.CurrentUser(r.Context()).ID
		if owner == "" {
			http.Error(w, "Sign in required.", http.StatusUnauthorized)
			return
		}
		h.serveRecent(lifetime, w, r, upgrader, owner, sessionUser)
	})
}

func (h *Handler) serveRecent(lifetime context.Context, w http.ResponseWriter, r *http.Request,
	upgrader websocket.Upgrader, owner string, sessionUser func(*http.Request) (*auth.User, error),
) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(lifetime, func() {
		cancel()
		_ = conn.Close()
	})
	defer stop()
	conn.SetReadLimit(1024)

	for {
		if err := conn.SetReadDeadline(time.Now().Add(45 * time.Second)); err != nil {
			return
		}
		kind, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var request sessionpb.RecentRequest
		if kind != websocket.BinaryMessage || proto.Unmarshal(payload, &request) != nil {
			closeSocket(conn, websocket.CloseUnsupportedData, "Invalid session list request.")
			return
		}
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		user, err := sessionUser(r.WithContext(requestCtx))
		if err != nil || user == nil || user.ID != owner {
			cancel()
			closeSocket(conn, websocket.ClosePolicyViolation, "Session unavailable.")
			return
		}
		data, err := h.recent(requestCtx, owner, request.ProjectId)
		cancel()
		if err != nil {
			h.logger.Error("encode session list", "error", err)
			closeSocket(conn, websocket.CloseInternalServerErr, "Session list unavailable.")
			return
		}
		if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
			return
		}
	}
}

func closeSocket(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}

func (h *Handler) recent(ctx context.Context, owner string, projectID *string) ([]byte, error) {
	projects, err := h.reader.Projects(ctx, owner)
	if err != nil {
		return nil, err
	}
	sessions, err := h.reader.RecentSessions(ctx, owner, projectID)
	if err != nil {
		return nil, err
	}
	page := &sessionpb.RecentPage{
		Projects: make([]*sessionpb.Project, len(projects)),
		Sessions: make([]*sessionpb.SessionSummary, len(sessions)),
	}
	for i, project := range projects {
		page.Projects[i] = &sessionpb.Project{Id: project.ID, Name: project.Name}
	}
	for i, session := range sessions {
		page.Sessions[i] = summary(session)
	}
	payload, err := proto.Marshal(page)
	if err != nil {
		return nil, err
	}
	var compressed bytes.Buffer
	writer := h.brotli.Get().(*brotli.Writer)
	writer.Reset(&compressed)
	if _, err := writer.Write(payload); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	h.brotli.Put(writer)
	return compressed.Bytes(), nil
}

func (h *Handler) Transcript(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := auth.CurrentUser(ctx).ID
	if owner == "" {
		http.Error(w, "Sign in required.", http.StatusUnauthorized)
		return
	}
	var request struct{ ID string }
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	if err := decoder.Decode(&request); err != nil || request.ID == "" || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		http.Error(w, "Invalid transcript request.", http.StatusBadRequest)
		return
	}
	transcript, err := h.reader.Transcript(ctx, owner, request.ID)
	if err != nil {
		h.transcriptError(w, err)
		return
	}
	settings, err := h.reader.Settings(ctx, owner)
	if err != nil {
		h.transcriptError(w, err)
		return
	}

	w.Header().Set("Content-Type", transcriptType)
	controller := http.NewResponseController(w)
	send := func(chunk *sessionpb.TranscriptChunk) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame, err := h.transcriptFrame(chunk)
		if err != nil {
			return err
		}
		if err := controller.SetWriteDeadline(time.Now().Add(socketTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if _, err := w.Write(frame); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := sendTranscript(transcript, settings, send); err != nil && ctx.Err() == nil {
		h.logger.Debug("stream transcript", "error", err)
	}
}

func (h *Handler) transcriptError(w http.ResponseWriter, err error) {
	h.logger.Error("read transcript", "error", err)
	http.Error(w, "The run service is unavailable.", http.StatusInternalServerError)
}

func (h *Handler) transcriptFrame(chunk *sessionpb.TranscriptChunk) ([]byte, error) {
	payload, err := proto.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	// Reserve four bytes for the compressed frame length in network byte order.
	frame := h.zstd.EncodeAll(payload, make([]byte, 4, 4+len(payload)/8))
	binary.BigEndian.PutUint32(frame, uint32(len(frame)-4))
	return frame, nil
}
