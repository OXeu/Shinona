package sessiontransfer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

const LivePath = "/api/v1/live"

const liveHeartbeat = 15 * time.Second

var errLiveAuth = errors.New("live session cookie is no longer valid")

type LiveSource interface {
	WatchSession(context.Context, string, string) (*run.SessionWatch, error)
}

type liveConnection struct {
	window   run.LiveTranscriptWindow
	known    map[string]run.TurnVersion
	progress run.LiveProgress
}

func (h *Handler) Live(lifetime context.Context, origin string, sessionUser func(*http.Request) (*auth.User, error), source LiveSource) http.Handler {
	return h.live(lifetime, origin, sessionUser, source, liveHeartbeat)
}

func (h *Handler) live(lifetime context.Context, origin string, sessionUser func(*http.Request) (*auth.User, error), source LiveSource, heartbeat time.Duration) http.Handler {
	upgrader := websocket.Upgrader{
		HandshakeTimeout:  socketTimeout,
		EnableCompression: true,
		CheckOrigin:       func(r *http.Request) bool { return r.Header.Get("Origin") == origin },
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := auth.CurrentUser(r.Context()).ID
		if owner == "" {
			httpserver.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "Sign in required.")
			return
		}
		conn, err := upgrader.Upgrade(w, r, httpserver.WebSocketTimingHeaders(w))
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		ctx, phases := timing.WithRecorder(ctx)
		stopLifetime := context.AfterFunc(lifetime, cancel)
		stopSocket := context.AfterFunc(ctx, func() { _ = conn.Close() })
		var readerDone <-chan struct{}
		defer func() {
			stopLifetime()
			stopSocket()
			cancel()
			_ = conn.Close()
			if readerDone != nil {
				<-readerDone
			}
			h.logSocketTimings("live", phases)
		}()
		request, err := readLiveRequest(conn)
		if err != nil {
			closeSocket(conn, websocket.CloseUnsupportedData, "Invalid live subscription.")
			return
		}
		readerDone, err = startLiveReader(conn, cancel, heartbeat)
		if err != nil {
			return
		}
		subscribe := timing.Start(ctx, "live_subscribe")
		watch, err := source.WatchSession(ctx, owner, request.SessionId)
		subscribe()
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return
			case errors.Is(err, run.ErrSession):
				closeSocket(conn, websocket.ClosePolicyViolation, "Session unavailable.")
			default:
				h.logger.Error("open live session", "error", err)
				closeSocket(conn, websocket.CloseInternalServerErr, "Session unavailable.")
			}
			return
		}
		defer watch.Close()
		known := make(map[string]run.TurnVersion, len(request.KnownTurns))
		for _, version := range request.KnownTurns {
			updated, err := time.Parse(time.RFC3339Nano, version.GetUpdatedAt())
			if err == nil {
				known[version.Id] = run.TurnVersion{ID: version.Id, UpdatedAt: updated, Status: run.Status(version.Status)}
			}
		}
		var sequence uint32
		var nextAuth time.Time
		send := func(message *sessionpb.LiveMessage) error {
			// Recheck at the heartbeat interval and before a frame if loading
			// data has already taken us beyond the next check.
			now := time.Now()
			if !now.Before(nextAuth) {
				authCtx, stop := context.WithTimeout(ctx, socketTimeout)
				user, err := sessionUser(r.WithContext(authCtx))
				stop()
				if err != nil {
					return err
				}
				if user == nil || user.ID != owner {
					return errLiveAuth
				}
				nextAuth = now.Add(heartbeat)
			}
			sequence++
			message.Sequence = sequence
			payload, err := encodeProtobuf(ctx, message)
			if err != nil {
				return err
			}
			if err := conn.SetWriteDeadline(time.Now().Add(socketTimeout)); err != nil {
				return err
			}
			defer timing.Start(ctx, "websocket_write")()
			return conn.WriteMessage(websocket.BinaryMessage, payload)
		}
		state := liveConnection{window: run.LiveLatestPage, known: known}
		if request.Paged != nil && !request.GetPaged() {
			state.window = run.LiveFullHistory
		}
		err = h.streamLive(ctx, conn, watch, &state, owner, request.SessionId, send, heartbeat)
		switch {
		case errors.Is(err, errLiveAuth):
			closeSocket(conn, websocket.ClosePolicyViolation, "Sign in required.")
		case err != nil && ctx.Err() == nil:
			h.logger.Debug("stream live session", "error", err)
			closeSocket(conn, websocket.CloseInternalServerErr, "Live session unavailable.")
		case err == nil:
			closeSocket(conn, websocket.CloseNormalClosure, "")
		}
	})
}

const socketTimeout = 5 * time.Second

func readLiveRequest(conn *websocket.Conn) (*sessionpb.LiveRequest, error) {
	const maxRequest = 64 << 10
	conn.SetReadLimit(maxRequest)
	if err := conn.SetReadDeadline(time.Now().Add(socketTimeout)); err != nil {
		return nil, err
	}
	kind, reader, err := conn.NextReader()
	if err != nil {
		return nil, err
	}
	if kind != websocket.BinaryMessage {
		return nil, errors.New("invalid live subscription frame")
	}
	// Gorilla's limit applies to bytes on the wire. Bound the decoded data too.
	payload, err := io.ReadAll(io.LimitReader(reader, maxRequest+1))
	if err != nil || len(payload) > maxRequest {
		return nil, errors.New("live subscription exceeds its size limit")
	}
	var request sessionpb.LiveRequest
	if proto.Unmarshal(payload, &request) != nil || request.SessionId == "" ||
		len(request.KnownTurns) > run.MaxLiveTranscriptTurns {
		return nil, errors.New("invalid live subscription request")
	}
	return &request, nil
}

func startLiveReader(conn *websocket.Conn, cancel context.CancelFunc, heartbeat time.Duration) (<-chan struct{}, error) {
	conn.SetReadLimit(125)
	extend := func() error { return conn.SetReadDeadline(time.Now().Add(3 * heartbeat)) }
	if err := extend(); err != nil {
		return nil, err
	}
	conn.SetPongHandler(func(string) error { return extend() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		// Only control frames are valid after the subscription request.
		_, _, _ = conn.NextReader()
	}()
	return done, nil
}

func (h *Handler) streamLive(ctx context.Context, conn *websocket.Conn, watch *run.SessionWatch, state *liveConnection,
	owner, id string, send func(*sessionpb.LiveMessage) error, heartbeat time.Duration,
) error {
	syncPage := func(phase string) (bool, error) {
		defer timing.Start(ctx, phase)()
		load := timing.Start(ctx, "live_load_transcript")
		update, err := h.reader.TranscriptUpdate(ctx, owner, id, state.window, state.known)
		load()
		if err != nil {
			return false, err
		}
		loadSettings := timing.Start(ctx, "sandbox_settings")
		settings, err := h.reader.Settings(ctx, owner)
		loadSettings()
		if err != nil {
			return false, err
		}
		build := timing.Start(ctx, "live_build_sync")
		snapshot := state.sync(update, settings)
		build()
		err = send(&sessionpb.LiveMessage{Item: &sessionpb.LiveMessage_Sync{Sync: snapshot}})
		return update != nil, err
	}
	if found, err := syncPage("live_bootstrap"); err != nil || !found {
		return err
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := send(&sessionpb.LiveMessage{Item: &sessionpb.LiveMessage_Heartbeat{Heartbeat: true}}); err != nil {
				return err
			}
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(socketTimeout)); err != nil {
				return err
			}
		case <-watch.Changes():
			change := watch.Read()
			if change.Transcript || change.Progress.TurnID != state.progress.TurnID {
				if found, err := syncPage("live_resync"); err != nil || !found {
					return err
				}
				continue
			}
			if change.Progress.TurnID == "" {
				continue
			}
			build := timing.Start(ctx, "live_build_delta")
			delta := progressDelta(state.progress, change.Progress)
			build()
			state.progress = change.Progress
			if delta != nil {
				write := timing.Start(ctx, "live_send_delta")
				err := send(&sessionpb.LiveMessage{Item: &sessionpb.LiveMessage_Progress{Progress: delta}})
				write()
				if err != nil {
					return err
				}
			}
		}
	}
}

func (s *liveConnection) sync(update *run.TranscriptUpdate, settings run.Settings) *sessionpb.LiveSync {
	result := &sessionpb.LiveSync{SandboxSettings: SandboxSettings(settings)}
	if update == nil {
		return result
	}
	result.Session = Summary(update.Session)
	result.LastOptions = RunOptions(update.LastOptions)
	result.NextBeforeSequence = int32(update.NextBeforeSequence)
	result.TurnIds = make([]string, 0, len(update.Versions))
	s.known = make(map[string]run.TurnVersion, len(update.Versions))
	for _, version := range update.Versions {
		result.TurnIds = append(result.TurnIds, version.ID)
		s.known[version.ID] = version
	}
	s.progress = run.LiveProgress{}
	for _, item := range update.ChangedTurns {
		if item.Status == run.Running {
			s.progress = run.LiveProgress{TurnID: item.ID, Parts: item.Parts, Steering: item.Steering, Context: item.LiveContext}
		}
		result.ChangedTurns = append(result.ChangedTurns, Turn(item))
	}
	return result
}
