// Package terminal connects authenticated browsers to retained sandbox PTYs.
package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
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

func Routes(mux httpserver.Routes, lifetime context.Context, origin string, source Source,
	requireUser, sameOriginJSON func(http.Handler) http.Handler,
	sessionUser func(*http.Request) (*auth.User, error), logger *slog.Logger,
) {
	h := handler{
		lifetime: lifetime, source: source, sessionUser: sessionUser, logger: logger,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: socketTimeout,
			CheckOrigin:      func(r *http.Request) bool { return r.Header.Get("Origin") == origin },
			Error: func(w http.ResponseWriter, _ *http.Request, status int, _ error) {
				httpserver.WriteError(w, status, "TERMINAL_UPGRADE_FAILED", "Could not open a terminal connection.")
			},
		},
	}
	mux.Handle("GET "+Path, requireUser(http.HandlerFunc(h.list)))
	mux.Handle("GET "+Path+"/connect", requireUser(http.HandlerFunc(h.connect)))
	mux.Handle("POST "+Path+"/create", sameOriginJSON(requireUser(http.HandlerFunc(h.create))))
	mux.Handle("POST "+Path+"/close", sameOriginJSON(requireUser(http.HandlerFunc(h.close))))
}

func (h handler) list(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	if !validID(session) {
		httpserver.WriteError(w, http.StatusBadRequest, "INVALID_TERMINAL_REQUEST", "Invalid conversation.")
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
	if err := httpserver.DecodeJSON(w, r, 1024, &request); err != nil ||
		!validID(request.Session) || (needsTerminal && !validID(request.Terminal)) {
		httpserver.WriteError(w, http.StatusBadRequest, "INVALID_TERMINAL_REQUEST", "Invalid terminal request.")
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
		httpserver.WriteError(w, http.StatusNotFound, "TERMINAL_NOT_FOUND", "Terminal conversation or tab not found.")
	case errors.Is(err, run.ErrTerminalLimit):
		httpserver.WriteError(w, http.StatusConflict, "TERMINAL_LIMIT", "Terminal limit reached.")
	case errors.Is(err, run.ErrWorkspaceBusy):
		httpserver.WriteError(w, http.StatusConflict, "WORKSPACE_BUSY", "Workspace is busy.")
	case errors.Is(err, run.ErrTerminalUnavailable):
		httpserver.WriteError(w, http.StatusServiceUnavailable, "TERMINAL_UNAVAILABLE", "Terminal unavailable.")
	default:
		h.logger.Warn("terminal request", "error", err)
		httpserver.WriteError(w, http.StatusInternalServerError, "TERMINAL_UNAVAILABLE", "Terminal unavailable.")
	}
}

func (h handler) connect(w http.ResponseWriter, r *http.Request) {
	owner := auth.CurrentUser(r.Context()).ID
	session, id := r.URL.Query().Get("session"), r.URL.Query().Get("terminal")
	if !validID(session) || !validID(id) {
		httpserver.WriteError(w, http.StatusBadRequest, "INVALID_TERMINAL_REQUEST", "Invalid terminal request.")
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
	request := r.Clone(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		_ = socket.Close()
		workers.Wait()
	}()
	workers.Go(func() { readInput(ctx, socket, tab, cancel) })
	workers.Go(func() {
		keepAlive(ctx, socket, cancel, h.logger, heartbeat, func(check context.Context) (bool, error) {
			user, err := h.sessionUser(request.WithContext(check))
			return user != nil && user.ID == owner, err
		})
	})
	if relay(ctx, socket, tab) == nil {
		sendClose(socket, websocket.CloseNormalClosure, "Terminal closed")
	}
}

func readInput(ctx context.Context, socket *websocket.Conn, tab *run.TerminalSession, cancel context.CancelFunc) {
	defer cancel()
	var payload bytes.Buffer
	for {
		kind, reader, err := socket.NextReader()
		if err != nil {
			return
		}
		payload.Reset()
		if _, err := payload.ReadFrom(reader); err != nil {
			return
		}
		var size sandbox.TerminalSize
		if kind == websocket.TextMessage && (json.Unmarshal(payload.Bytes(), &size) != nil || !size.Valid()) {
			sendClose(socket, websocket.CloseUnsupportedData, "Invalid terminal resize")
			return
		}
		request, stop := context.WithTimeout(ctx, socketTimeout)
		switch kind {
		case websocket.BinaryMessage:
			input := timing.Start(request, "pty_input")
			err = tab.Input(request, payload.Bytes())
			input()
		case websocket.TextMessage:
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
	replayEnd := tab.ReplayEnd()
	for ctx.Err() == nil {
		var boundary *uint64
		if offset < replayEnd {
			boundary = &replayEnd
		}
		next, n, state, changed := tab.Output(offset, output[:], boundary)
		offset = next
		if boundary != nil && n == 0 && offset >= replayEnd {
			// The replay was overwritten. Announce the state before reading
			// any still-retained live output.
			continue
		}
		exiting := state == run.TerminalExited || state == run.TerminalFailed
		// Announce running after the replay boundary even under continuous
		// output. Announce exit only after delivering the final output.
		if state != reported && ((boundary == nil && !exiting) || n == 0) {
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
		if exiting {
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

func keepAlive(ctx context.Context, socket *websocket.Conn, cancel context.CancelFunc,
	logger *slog.Logger, interval time.Duration, authorized func(context.Context) (bool, error),
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check, stop := context.WithTimeout(ctx, socketTimeout)
			ok, err := authorized(check)
			stop()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				logger.Warn("verify terminal session", "error", err)
				sendClose(socket, websocket.CloseInternalServerErr, "Could not verify terminal session")
				return
			}
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
