// Package httpauth exposes the authentication service over HTTP.
package httpauth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/timing"
)

const sessionCookie = "shinona_session"
const flowCookie = "shinona_oauth"
const linkFlowPrefix = "link:"

type OAuthProvider interface {
	AuthorizationURL(string, auth.Flow) string
	Exchange(context.Context, string, auth.Flow) (auth.Identity, error)
}

type Config struct {
	OAuth          OAuthProvider
	Mailer         auth.VerificationMailer
	Secure         bool
	FlowVersion    string
	AccountDeleted func(string)
	TrustedProxies []netip.Prefix
}

type Limits struct {
	PasswordConcurrency int
	RateLimitEntries    int
}

func DefaultLimits() Limits {
	return Limits{PasswordConcurrency: 2, RateLimitEntries: 8192}
}

type Handler struct {
	service       *auth.Service
	config        func() Config
	logger        *slog.Logger
	loginRate     requestRate[netip.Addr]
	oauthRate     requestRate[netip.Addr]
	accountRate   requestRate[string]
	passwordSlots chan struct{}
}

func New(service *auth.Service, oauth OAuthProvider, secure bool, logger *slog.Logger) *Handler {
	return NewWithConfig(service, func() Config { return Config{OAuth: oauth, Secure: secure} }, DefaultLimits(), logger)
}

func NewWithConfig(service *auth.Service, config func() Config, limits Limits, logger *slog.Logger) *Handler {
	return &Handler{
		service: service, config: config, logger: logger,
		loginRate:     newRequestRate[netip.Addr](limits.RateLimitEntries),
		oauthRate:     newRequestRate[netip.Addr](limits.RateLimitEntries),
		accountRate:   newRequestRate[string](limits.RateLimitEntries),
		passwordSlots: make(chan struct{}, limits.PasswordConcurrency),
	}
}

func (h *Handler) Routes(mux httpserver.Routes, post func(http.Handler) http.Handler) {
	mux.HandleFunc("GET /auth/session", h.session)
	mux.Handle("POST /auth/password", post(http.HandlerFunc(h.password)))
	mux.Handle("POST /auth/logout", post(http.HandlerFunc(h.logout)))
	mux.HandleFunc("GET /auth/oauth/start", h.oauthStart)
	mux.HandleFunc("GET /auth/oauth/callback", h.oauthCallback)
	h.accountRoutes(mux, post)
}

func (h *Handler) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if request := h.authenticated(w, r); request != nil {
			next.ServeHTTP(w, request)
		}
	})
}

func (h *Handler) RequireGinUser(c *gin.Context) {
	request := h.authenticated(c.Writer, c.Request)
	if request == nil {
		c.Abort()
		return
	}
	c.Request = request
	c.Next()
}

func (h *Handler) authenticated(w http.ResponseWriter, r *http.Request) *http.Request {
	w.Header().Set("Cache-Control", "no-store")
	user, err := h.SessionUser(r)
	if err != nil {
		h.failure(w, err)
		return nil
	}
	if user == nil {
		authError(w, "AUTH_REQUIRED", "Sign in required.", http.StatusUnauthorized)
		return nil
	}
	httpserver.AllowBusinessTiming(w)
	return r.WithContext(auth.WithUser(r.Context(), *user))
}

type sessionResponse struct {
	User         *userResponse `json:"user"`
	OAuthEnabled bool          `json:"oauthEnabled"`
	EmailEnabled bool          `json:"emailEnabled"`
}

type userResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	LoginEmail  string `json:"loginEmail"`
	Name        string `json:"name"`
	AvatarURL   string `json:"avatarUrl,omitempty"`
	HasAvatar   bool   `json:"hasAvatar"`
	HasPassword bool   `json:"hasPassword"`
	Admin       bool   `json:"admin"`
	Disabled    bool   `json:"disabled"`
}

func publicUser(user auth.User) userResponse {
	avatarURL := user.AvatarURL
	if user.AvatarVersion != "" {
		avatarURL = "/auth/account/avatar/" + url.PathEscape(user.ID) + "/" + url.PathEscape(user.AvatarVersion)
	}
	return userResponse{ID: user.ID, Email: user.Email, LoginEmail: user.LoginEmail, Name: user.Name,
		AvatarURL: avatarURL, HasAvatar: user.AvatarVersion != "", HasPassword: user.HasPassword, Admin: user.Admin, Disabled: user.Disabled}
}

func (h *Handler) respond(w http.ResponseWriter, user *auth.User) {
	config := h.config()
	payload := sessionResponse{OAuthEnabled: config.OAuth != nil, EmailEnabled: config.Mailer != nil}
	if user != nil {
		account := publicUser(*user)
		payload.User = &account
	}
	h.writeJSON(w, payload)
}

func (h *Handler) writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		h.logger.Debug("write auth response", "error", err)
	}
}

func (h *Handler) session(w http.ResponseWriter, r *http.Request) {
	user, err := h.SessionUser(r)
	if err != nil {
		h.failure(w, err)
		return
	}
	h.respond(w, user)
}

func (h *Handler) password(w http.ResponseWriter, r *http.Request) {
	if !h.loginRate.allow(loginAddress(r, h.config().TrustedProxies), time.Now()) {
		authError(w, "AUTH_RATE_LIMITED", "Too many sign-in attempts. Try again shortly.", http.StatusTooManyRequests)
		return
	}
	var input struct{ Email, Password string }
	if err := httpserver.DecodeJSON(w, r, 4096, &input); err != nil {
		authError(w, "AUTH_INVALID_REQUEST", "Invalid sign-in request.", http.StatusBadRequest)
		return
	}

	select {
	case h.passwordSlots <- struct{}{}:
	default:
		authError(w, "AUTH_BUSY", "Sign-in is busy. Try again shortly.", http.StatusTooManyRequests)
		return
	}
	// Keep the lookup and verification phases private until sign-in succeeds.
	ctx, phases := timing.WithRecorder(r.Context())
	user, err := func() (auth.User, error) {
		defer func() { <-h.passwordSlots }()
		return h.service.PasswordUser(ctx, input.Email, input.Password)
	}()
	if errors.Is(err, auth.ErrCredentials) {
		authError(w, "AUTH_CREDENTIALS", "Email or password is incorrect.", http.StatusUnauthorized)
		return
	}
	if err != nil {
		h.failure(w, err)
		return
	}
	if err := h.signIn(w, r.WithContext(ctx), user, nil); err != nil {
		h.failure(w, err)
		return
	}
	phases.Publish(r.Context())
	httpserver.AllowBusinessTiming(w)
	h.respond(w, &user)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := h.service.EndSession(r.Context(), readCookie(r, sessionCookie)); err != nil {
		h.failure(w, err)
		return
	}
	h.cookie(w, sessionCookie, "/", "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) oauthStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	config := h.config()
	if config.OAuth == nil || !h.oauthRate.allow(loginAddress(r, config.TrustedProxies), time.Now()) {
		http.Redirect(w, r, "/login?oauth=failed", http.StatusSeeOther)
		return
	}
	if err := h.cancelPreviousOAuthFlow(r); err != nil {
		h.failure(w, err)
		return
	}
	state, flow, err := h.service.BeginFlow(r.Context(), config.FlowVersion)
	if err != nil {
		h.failure(w, err)
		return
	}
	h.cookie(w, flowCookie, "/auth/oauth", state, int(auth.FlowLifetime.Seconds()))
	http.Redirect(w, r, config.OAuth.AuthorizationURL(state, flow), http.StatusSeeOther)
}

func (h *Handler) cancelPreviousOAuthFlow(r *http.Request) error {
	previous, _ := strings.CutPrefix(readCookie(r, flowCookie), linkFlowPrefix)
	if previous == "" {
		return nil
	}
	_, err := h.service.ConsumeFlow(r.Context(), previous)
	return err
}

func (h *Handler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	state := r.URL.Query().Get("state")
	cookie, link := strings.CutPrefix(readCookie(r, flowCookie), linkFlowPrefix)
	config := h.config()
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(cookie)) != 1 {
		h.redirectOAuthResult(w, r, link, auth.ErrCredentials)
		return
	}
	h.cookie(w, flowCookie, "/auth/oauth", "", -1)
	err := h.finishOAuth(w, r, state, config, link)
	if err != nil {
		h.logger.Warn("OAuth callback failed", "link", link, "failure", httpserver.OAuthFailure(err))
	}
	h.redirectOAuthResult(w, r, link, err)
}

func (h *Handler) redirectOAuthResult(w http.ResponseWriter, r *http.Request, link bool, err error) {
	target := "/"
	switch {
	case link && err == nil:
		target = "/account?oauthLink=linked"
	case link && errors.Is(err, auth.ErrOAuthInUse):
		target = "/account?oauthLink=in-use"
	case link:
		target = "/account?oauthLink=failed"
	case err != nil:
		target = "/login?oauth=failed"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *Handler) finishOAuth(w http.ResponseWriter, r *http.Request, state string, config Config, link bool) error {
	ctx := r.Context()
	flow, err := h.service.ConsumeFlow(ctx, state)
	if err != nil {
		return err
	}
	code := r.URL.Query().Get("code")
	if config.OAuth == nil || flow == nil || code == "" || flow.Version != config.FlowVersion || link != (flow.OwnerID != "") {
		return auth.ErrCredentials
	}
	token := readCookie(r, sessionCookie)
	if link && !flow.MatchesSession(token) {
		return auth.ErrCredentials
	}
	identity, err := config.OAuth.Exchange(ctx, code, *flow)
	if err != nil {
		return err
	}
	if link {
		return h.service.FinishOAuthLink(ctx, *flow, token, identity)
	}
	user, err := h.service.OAuthUser(ctx, identity)
	if err != nil {
		return err
	}
	return h.signIn(w, r, user, &identity)
}

func (h *Handler) signIn(w http.ResponseWriter, r *http.Request, user auth.User, identity *auth.Identity) error {
	if err := h.service.EndSession(r.Context(), readCookie(r, sessionCookie)); err != nil {
		return err
	}
	var token string
	var err error
	if identity != nil {
		token, err = h.service.StartOAuthSession(r.Context(), user, *identity)
	} else {
		token, err = h.service.StartSession(r.Context(), user)
	}
	if err != nil {
		return err
	}
	h.cookie(w, sessionCookie, "/", token, int(auth.SessionLifetime.Seconds()))
	return nil
}

// SessionUser resolves the current cookie, including expiry and revocation.
func (h *Handler) SessionUser(r *http.Request) (*auth.User, error) {
	defer timing.Start(r.Context(), "auth_session")()
	return h.service.SessionUser(r.Context(), readCookie(r, sessionCookie))
}

func (h *Handler) cookie(w http.ResponseWriter, name, path, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: h.config().Secure, SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) failure(w http.ResponseWriter, err error) {
	h.logger.Error("authentication request failed", "error", err)
	authError(w, "AUTH_UNAVAILABLE", "Authentication is unavailable.", http.StatusInternalServerError)
}

func readCookie(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func authError(w http.ResponseWriter, code, message string, status int) {
	httpserver.WriteError(w, status, code, message)
}
