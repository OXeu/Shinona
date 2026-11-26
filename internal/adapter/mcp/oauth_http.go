package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/settings"
	"github.com/OXeu/shinona/internal/timing"
)

const oauthCookie = "shinona_mcp_oauth"

type oauthHandler struct {
	oauth  *OAuth
	lookup oauthServerLookup
	secure bool
	logger *slog.Logger
}

func (o *OAuth) Routes(mux httpserver.Routes, requireUser, sameOrigin func(http.Handler) http.Handler,
	lookup oauthServerLookup, secure bool, logger *slog.Logger,
) {
	h := oauthHandler{oauth: o, lookup: lookup, secure: secure, logger: logger}
	mux.Handle("POST /auth/mcp/start", sameOrigin(requireUser(http.HandlerFunc(h.start))))
	mux.Handle("POST /auth/mcp/disconnect", sameOrigin(requireUser(http.HandlerFunc(h.disconnect))))
	mux.Handle("GET "+OAuthCallback, requireUser(http.HandlerFunc(h.callback)))
	mux.HandleFunc("GET "+OAuthMetadata, h.metadata)
}

func submittedName(w http.ResponseWriter, r *http.Request) (string, bool) {
	var input struct{ Name string }
	if err := httpserver.DecodeJSON(w, r, 4096, &input); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "INVALID_MCP_OAUTH", "Invalid MCP OAuth request.")
		return "", false
	}
	return input.Name, true
}

func (h oauthHandler) findServer(ctx context.Context, owner, name string) (settings.MCPServer, bool) {
	defer timing.Start(ctx, "mcp_server_lookup")()
	return h.lookup(ctx, owner, name)
}

func (h oauthHandler) start(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name, ok := submittedName(w, r)
	if !ok {
		return
	}
	state, destination, err := h.oauth.Start(r.Context(), auth.CurrentUser(r.Context()).ID, name, stateCookie(r), h.findServer)
	if err != nil {
		if errors.Is(err, errOAuthServerNotFound) {
			httpserver.WriteError(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", "MCP OAuth server not found.")
			return
		}
		if errors.Is(err, ErrOAuthClientRequired) {
			httpserver.WriteError(w, http.StatusBadRequest, "MCP_OAUTH_CLIENT_REQUIRED", ErrOAuthClientRequired.Error())
			return
		}
		if errors.Is(err, ErrOAuthFlow) {
			httpserver.WriteError(w, http.StatusConflict, "MCP_OAUTH_START_FAILED", ErrOAuthFlow.Error())
			return
		}
		h.logger.Warn("start MCP OAuth failed", "server", name, "failure", httpserver.OAuthFailure(err))
		httpserver.WriteError(w, http.StatusBadGateway, "MCP_OAUTH_START_FAILED", "Could not start MCP OAuth.")
		return
	}
	h.cookie(w, state, int(OAuthFlowLifetime.Seconds()))
	writeOAuthJSON(w, map[string]string{"url": destination})
}

func (h oauthHandler) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	query := r.URL.Query()
	state := query.Get("state")
	cookie := stateCookie(r)
	h.cookie(w, "", -1)
	result := "failed"
	if state != "" && subtle.ConstantTimeCompare([]byte(cookie), []byte(state)) == 1 {
		owner := auth.CurrentUser(r.Context()).ID
		err := h.oauth.Finish(r.Context(), owner, state, query.Get("code"), query.Get("iss"), h.findServer)
		if err == nil {
			result = "connected"
		} else if !errors.Is(err, ErrOAuthFlow) {
			h.logger.Warn("MCP OAuth callback failed", "failure", httpserver.OAuthFailure(err))
		}
	}
	http.Redirect(w, r, "/settings?mcpOAuth="+result, http.StatusSeeOther)
}

func (h oauthHandler) disconnect(w http.ResponseWriter, r *http.Request) {
	name, ok := submittedName(w, r)
	if !ok {
		return
	}
	ctx, owner := r.Context(), auth.CurrentUser(r.Context()).ID
	if server, ok := h.findServer(ctx, owner, name); !ok || server.OAuth == nil {
		httpserver.WriteError(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", "MCP OAuth server not found.")
		return
	}
	if err := h.oauth.Disconnect(ctx, owner, name); err != nil {
		h.logger.Error("disconnect MCP OAuth failed", "server", name, "error", err)
		httpserver.WriteError(w, http.StatusInternalServerError, "MCP_OAUTH_DISCONNECT_FAILED", "Could not disconnect MCP OAuth.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h oauthHandler) metadata(w http.ResponseWriter, r *http.Request) {
	lookup := timing.Start(r.Context(), "mcp_client_metadata")
	metadata := h.oauth.ClientMetadata()
	lookup()
	if metadata == nil {
		httpserver.WriteError(w, http.StatusNotFound, "MCP_CLIENT_METADATA_NOT_FOUND", "MCP client metadata not found.")
		return
	}
	writeOAuthJSON(w, metadata)
}

func (h oauthHandler) cookie(w http.ResponseWriter, state string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: state, MaxAge: maxAge,
		Path: "/auth/mcp", HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode})
}

func stateCookie(r *http.Request) string {
	cookie, err := r.Cookie(oauthCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func writeOAuthJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
