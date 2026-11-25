// Package terminal connects authenticated browsers to retained sandbox PTYs.
package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
)

const Path = "/api/terminal/v1"

const (
	socketTimeout = 5 * time.Second
	heartbeat     = 15 * time.Second
	signIn        = 4401
)

type Source interface {
	Terminals(context.Context, string, string) ([]run.TerminalTab, error)
	StartTerminal(context.Context, context.Context, string, string) (run.TerminalTab, error)
	Terminal(context.Context, string, string, string) (*run.TerminalSession, error)
	StopTerminal(context.Context, string, string, string) error
}

type handler struct {
	lifetime    context.Context
	source      Source
	sessionUser func(*http.Request) (*auth.User, error)
	logger      *slog.Logger
	upgrader    websocket.Upgrader
}

func Routes(mux *http.ServeMux, lifetime context.Context, origin string, source Source,
	requireUser, sameOriginJSON func(http.Handler) http.Handler,
	sessionUser func(*http.Request) (*auth.User, error), logger *slog.Logger,
) {
	h := handler{
		lifetime: lifetime, source: source, sessionUser: sessionUser, logger: logger,
		upgrader: websocket.Upgrader{HandshakeTimeout: socketTimeout,
			CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == origin }},
	}
	mux.Handle("GET "+Path, requireUser(http.HandlerFunc(h.list)))
	mux.Handle("GET "+Path+"/connect", requireUser(http.HandlerFunc(h.connect)))
	mux.Handle("POST "+Path+"/create", sameOriginJSON(requireUser(http.HandlerFunc(h.create))))
	mux.Handle("POST "+Path+"/close", sameOriginJSON(requireUser(http.HandlerFunc(h.close))))
}

func (h handler) list(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	if !validID(session) {
		http.Error(w, "Invalid conversation", http.StatusBadRequest)
		return
	}
	list := timing.Start(r.Context(), "terminal_list")
	tabs, err := h.source.Terminals(r.Context(), auth.CurrentUser(r.Context()).ID, session)
	list()
	if err != nil {
		h.respondError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, tabs)
}

type terminalRequest struct {
	Session  string `json:"session"`
	Terminal string `json:"terminal"`
}

func readRequest(w http.ResponseWriter, r *http.Request, needsTerminal bool) (terminalRequest, bool) {
	var request terminalRequest
	decoder := json.NewDecoder(httpserver.MaxBytesReader(w, r.Body, 1024))
	if err := decoder.Decode(&request); err != nil ||
		!validID(request.Session) || (needsTerminal && !validID(request.Terminal)) ||
		!errors.Is(decoder.Decode(new(any)), io.EOF) {
		http.Error(w, "Invalid terminal request", http.StatusBadRequest)
		return terminalRequest{}, false
	}
	return request, true
}

func validID(id string) bool { return id != "" && len(id) <= 128 }

func (h handler) create(w http.ResponseWriter, r *http.Request) {
	request, ok := readRequest(w, r, false)
	if !ok {
		return
	}
	start := timing.Start(r.Context(), "terminal_register")
	tab, err := h.source.StartTerminal(r.Context(), h.lifetime, auth.CurrentUser(r.Context()).ID, request.Session)
	start()
	if err != nil {
		h.respondError(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, tab)
}

func (h handler) close(w http.ResponseWriter, r *http.Request) {
	request, ok := readRequest(w, r, true)
	if !ok {
		return
	}
	stop := timing.Start(r.Context(), "terminal_close")
	err := h.source.StopTerminal(r.Context(), auth.CurrentUser(r.Context()).ID, request.Session, request.Terminal)
	stop()
	if err != nil && !errors.Is(err, run.ErrTerminalNotFound) {
		h.respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h handler) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		h.logger.Debug("write terminal response", "error", err)
	}
}

func (h handler) respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, run.ErrSession), errors.Is(err, run.ErrTerminalNotFound):
		http.Error(w, "Terminal conversation or tab not found", http.StatusNotFound)
	case errors.Is(err, run.ErrTerminalLimit):
		w.Header().Set("X-Shinona-Error-Code", "TERMINAL_LIMIT")
		http.Error(w, "Terminal limit reached", http.StatusConflict)
	case errors.Is(err, run.ErrTerminalUnavailable):
		http.Error(w, "Terminal unavailable", http.StatusServiceUnavailable)
	default:
		h.logger.Warn("terminal request", "error", err)
		http.Error(w, "Terminal unavailable", http.StatusInternalServerError)
	}
}

func (h handler) connect(w http.ResponseWriter, r *http.Request) {
	owner := auth.CurrentUser(r.Context()).ID
	session, id := r.URL.Query().Get("session"), r.URL.Query().Get("terminal")
	if !validID(session) || !validID(id) {
		http.Error(w, "Invalid terminal request", http.StatusBadRequest)
		return
	}
	lookup := timing.Start(r.Context(), "terminal_lookup")
	tab, err := h.source.Terminal(r.Context(), owner, session, id)
	lookup()
	if err != nil {
		h.respondError(w, err)
		return
	}
	socket, err := h.upgrader.Upgrade(w, r, httpserver.WebSocketTimingHeaders(w))
	if err != nil {
		return
	}
	defer func() { _ = socket.Close() }()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ctx, phases := timing.WithRecorder(ctx)
	defer func() {
		values := phases.LogValue()
		if len(values.Group()) > 0 {
			h.logger.Info("terminal WebSocket timing", "phases", values)
		}
	}()
	stop := context.AfterFunc(h.lifetime, func() {
		cancel()
		_ = socket.Close()
	})
	defer stop()
	socket.SetReadLimit(sandbox.TerminalChunkSize)
	refresh := func() error { return socket.SetReadDeadline(time.Now().Add(3 * heartbeat)) }
	if refresh() != nil {
		return
	}
	socket.SetPongHandler(func(string) error { return refresh() })
	go readInput(ctx, socket, tab, cancel)
	go keepAlive(ctx, socket, cancel, func(check context.Context) bool {
		user, err := h.sessionUser(r.WithContext(check))
		return err == nil && user != nil && user.ID == owner
	})
	if relay(ctx, socket, tab) == nil {
		sendClose(socket, websocket.CloseNormalClosure, "Terminal closed")
	}
}

func readInput(ctx context.Context, socket *websocket.Conn, tab *run.TerminalSession, cancel context.CancelFunc) {
	defer cancel()
	for {
		kind, payload, err := socket.ReadMessage()
		if err != nil {
			return
		}
		request, stop := context.WithTimeout(ctx, socketTimeout)
		switch kind {
		case websocket.BinaryMessage:
			input := timing.Start(request, "pty_input")
			err = tab.Input(request, payload)
			input()
		case websocket.TextMessage:
			var size sandbox.TerminalSize
			if json.Unmarshal(payload, &size) != nil || !size.Valid() {
				stop()
				sendClose(socket, websocket.CloseUnsupportedData, "Invalid terminal resize")
				return
			}
			resize := timing.Start(request, "pty_resize")
			err = tab.Resize(request, size)
			resize()
		}
		stop()
		// An input can cross the final output. Let the relay drain that output.
		if err != nil && !errors.Is(err, run.ErrTerminalUnavailable) {
			return
		}
	}
}

func relay(ctx context.Context, socket *websocket.Conn, tab *run.TerminalSession) error {
	var offset uint64
	var reported run.TerminalState
	var output [sandbox.TerminalChunkSize]byte
	for ctx.Err() == nil {
		next, n, state, changed := tab.Output(offset, output[:])
		offset = next
		// Replay prior output before announcing an exit.
		if state != reported && n == 0 {
			payload, err := json.Marshal(struct {
				State run.TerminalState `json:"state"`
			}{state})
			if err != nil {
				return err
			}
			if err := write(ctx, socket, websocket.TextMessage, payload); err != nil {
				return err
			}
			reported = state
		}
		if n > 0 {
			if err := write(ctx, socket, websocket.BinaryMessage, output[:n]); err != nil {
				return err
			}
			continue
		}
		if state == run.TerminalExited || state == run.TerminalFailed {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-changed:
		}
	}
	return ctx.Err()
}

func write(ctx context.Context, socket *websocket.Conn, kind int, data []byte) error {
	defer timing.Start(ctx, "websocket_write")()
	if err := socket.SetWriteDeadline(time.Now().Add(socketTimeout)); err != nil {
		return err
	}
	return socket.WriteMessage(kind, data)
}

func keepAlive(ctx context.Context, socket *websocket.Conn, cancel context.CancelFunc, authorized func(context.Context) bool) {
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check, stop := context.WithTimeout(ctx, socketTimeout)
			ok := authorized(check)
			stop()
			if !ok {
				sendClose(socket, signIn, "Sign in required")
				return
			}
			if socket.WriteControl(websocket.PingMessage, nil, time.Now().Add(socketTimeout)) != nil {
				return
			}
		}
	}
}

func sendClose(socket *websocket.Conn, code int, reason string) {
	_ = socket.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(socketTimeout))
}
