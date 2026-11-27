// Package sessiontransfer streams live session updates over WebSocket.
package sessiontransfer

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

var errLiveMessageTooLarge = errors.New("live update exceeds its size limit")

type Reader interface {
	TranscriptUpdate(context.Context, string, string, run.LiveTranscriptWindow, map[string]run.TurnVersion) (*run.TranscriptUpdate, error)
	Settings(context.Context, string) (run.Settings, error)
}

type Handler struct {
	reader Reader
	logger *slog.Logger
}

func New(reader Reader, logger *slog.Logger) *Handler {
	return &Handler{reader: reader, logger: logger}
}

func (h *Handler) logSocketTimings(operation string, phases *timing.Recorder) {
	values := phases.LogValue()
	if len(values.Group()) > 0 {
		h.logger.Info("session transfer timing", "operation", operation, "phases", values)
	}
}

func closeSocket(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}

func encodeProtobuf(ctx context.Context, message proto.Message) ([]byte, error) {
	defer timing.Start(ctx, "protobuf_encode")()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if proto.Size(message) > run.MaxLiveMessageBytes {
		return nil, errLiveMessageTooLarge
	}
	return proto.Marshal(message)
}
