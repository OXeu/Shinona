package sessiontransfer

import (
	"context"
	"errors"
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

type turnVersion struct{ updatedAt, status string }

type liveConnection struct {
	paged    bool
	known    map[string]turnVersion
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
			http.Error(w, "Sign in required.", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, httpserver.WebSocketTimingHeaders(w))
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		ctx, phases := timing.WithRecorder(ctx)
		defer h.logSocketTimings("live", phases)
		stop := context.AfterFunc(lifetime, func() {
			cancel()
			_ = conn.Close()
		})
		defer stop()
		request, err := readLiveRequest(conn)
		if err != nil {
			closeSocket(conn, websocket.CloseUnsupportedData, "Invalid live subscription.")
			return
		}
		subscribe := timing.Start(ctx, "live_subscribe")
		watch, err := source.WatchSession(ctx, owner, request.SessionId)
		subscribe()
		if err != nil {
			if errors.Is(err, run.ErrSession) {
				closeSocket(conn, websocket.ClosePolicyViolation, "Session unavailable.")
			} else {
				h.logger.Error("open live session", "error", err)
				closeSocket(conn, websocket.CloseInternalServerErr, "Session unavailable.")
			}
			return
		}
		defer watch.Close()
		if err := startLiveReader(conn, cancel, heartbeat); err != nil {
			return
		}
		known := make(map[string]turnVersion, len(request.KnownTurns))
		for _, version := range request.KnownTurns {
			known[version.Id] = turnVersion{version.UpdatedAt, version.Status}
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
		state := liveConnection{paged: request.Paged == nil || *request.Paged, known: known}
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
	conn.SetReadLimit(16 << 20)
	if err := conn.SetReadDeadline(time.Now().Add(socketTimeout)); err != nil {
		return nil, err
	}
	kind, payload, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	var request sessionpb.LiveRequest
	if kind != websocket.BinaryMessage || proto.Unmarshal(payload, &request) != nil || request.SessionId == "" {
		return nil, errors.New("invalid live subscription request")
	}
	return &request, nil
}

func startLiveReader(conn *websocket.Conn, cancel context.CancelFunc, heartbeat time.Duration) error {
	extend := func() error { return conn.SetReadDeadline(time.Now().Add(3 * heartbeat)) }
	if err := extend(); err != nil {
		return err
	}
	conn.SetPongHandler(func(string) error { return extend() })
	go func() {
		defer cancel()
		// Only control frames are valid after the subscription request.
		_, _, _ = conn.ReadMessage()
	}()
	return nil
}

func (h *Handler) streamLive(ctx context.Context, conn *websocket.Conn, watch *run.SessionWatch, state *liveConnection,
	owner, id string, send func(*sessionpb.LiveMessage) error, heartbeat time.Duration,
) error {
	syncPage := func(phase string) (bool, error) {
		defer timing.Start(ctx, phase)()
		load := timing.Start(ctx, "live_load_transcript")
		var page *run.Transcript
		var err error
		if state.paged {
			page, err = h.reader.TranscriptPage(ctx, owner, id, 0)
		} else {
			page, err = h.reader.FullTranscript(ctx, owner, id)
		}
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
		snapshot := state.sync(page, settings)
		build()
		err = send(&sessionpb.LiveMessage{Item: &sessionpb.LiveMessage_Sync{Sync: snapshot}})
		return page != nil, err
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

func (s *liveConnection) sync(page *run.Transcript, settings run.Settings) *sessionpb.LiveSync {
	result := &sessionpb.LiveSync{SandboxSettings: SandboxSettings(settings)}
	if page == nil {
		return result
	}
	result.Session = Summary(page.Session)
	result.LastOptions = RunOptions(page.LastOptions())
	result.NextBeforeSequence = int32(page.NextBeforeSequence)
	result.TurnIds = make([]string, 0, len(page.Turns))
	next := make(map[string]turnVersion, len(page.Turns))
	s.progress = run.LiveProgress{}
	for _, item := range page.Turns {
		version := turnVersion{item.UpdatedAt.Format(time.RFC3339Nano), string(item.Status)}
		result.TurnIds = append(result.TurnIds, item.ID)
		next[item.ID] = version
		if item.Status == run.Running {
			s.progress = run.LiveProgress{TurnID: item.ID, Parts: item.Parts, Steering: item.Steering, Context: item.LiveContext}
		}
		if known, exists := s.known[item.ID]; !exists || known != version || item.Status == run.Running {
			result.ChangedTurns = append(result.ChangedTurns, Turn(item))
		}
	}
	s.known = next
	return result
}
