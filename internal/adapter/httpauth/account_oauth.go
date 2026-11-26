package httpauth

import (
	"net/http"
	"net/url"

	"github.com/OXeu/shinona/internal/auth"
)

func (h *Handler) oauthStatus(w http.ResponseWriter, r *http.Request) {
	linked, err := h.service.HasOAuthIdentity(r.Context())
	if err != nil {
		h.accountError(w, err)
		return
	}
	h.writeJSON(w, struct {
		Linked bool `json:"linked"`
	}{linked})
}

func (h *Handler) startOAuthLink(w http.ResponseWriter, r *http.Request) {
	config := h.config()
	if config.OAuth == nil {
		h.accountError(w, auth.ErrOAuthDisabled)
		return
	}
	var input struct{ CurrentPassword string }
	if !accountInput(w, r, &input, 4096) || !h.accountPasswordSlot(w, r) {
		return
	}
	defer func() { <-h.passwordSlots }()
	if err := h.cancelPreviousOAuthFlow(r); err != nil {
		h.accountError(w, err)
		return
	}
	state, flow, err := h.service.BeginOAuthLink(r.Context(), config.FlowVersion, readCookie(r, sessionCookie), input.CurrentPassword)
	if err != nil {
		h.accountError(w, err)
		return
	}
	target, err := url.Parse(config.OAuth.AuthorizationURL(state, flow))
	if err != nil || target.Hostname() == "" || target.User != nil || (target.Scheme != "http" && target.Scheme != "https") {
		h.accountError(w, auth.ErrOAuthDisabled)
		return
	}
	h.cookie(w, flowCookie, "/auth/oauth", linkFlowPrefix+state, int(auth.FlowLifetime.Seconds()))
	h.writeJSON(w, struct {
		URL string `json:"url"`
	}{target.String()})
}

func (h *Handler) unlinkOAuth(w http.ResponseWriter, r *http.Request) {
	var input struct{ CurrentPassword string }
	if !accountInput(w, r, &input, 4096) || !h.accountPasswordSlot(w, r) {
		return
	}
	defer func() { <-h.passwordSlots }()
	err := h.service.UnlinkOAuth(r.Context(), readCookie(r, sessionCookie), input.CurrentPassword)
	h.accountSession(w, r, err)
}
