package httpauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/auth"
)

const adminPageSize = 25

func (h *Handler) accountRoutes(mux *http.ServeMux, post func(http.Handler) http.Handler) {
	accountPost := func(handler http.HandlerFunc) http.Handler { return post(h.RequireUser(handler)) }
	mux.Handle("POST /auth/account/profile", accountPost(h.profile))
	mux.Handle("GET /auth/account/avatar/{owner}/{version}", h.RequireUser(http.HandlerFunc(h.avatar)))
	mux.Handle("POST /auth/account/password", accountPost(h.changePassword))
	mux.Handle("POST /auth/account/email/start", accountPost(h.startEmailBinding))
	mux.Handle("POST /auth/account/email/verify", accountPost(h.verifyEmailBinding))
	mux.Handle("GET /auth/account/oauth", h.RequireUser(http.HandlerFunc(h.oauthStatus)))
	mux.Handle("POST /auth/oauth/link", accountPost(h.startOAuthLink))
	mux.Handle("POST /auth/account/oauth/unlink", accountPost(h.unlinkOAuth))
	mux.Handle("GET /auth/admin/users", h.RequireUser(http.HandlerFunc(h.accounts)))
	mux.Handle("POST /auth/admin/users", accountPost(h.createAccount))
	mux.Handle("POST /auth/admin/users/{id}/access", accountPost(h.changeAccess))
	mux.Handle("POST /auth/admin/users/{id}/delete", accountPost(h.deleteAccount))
}

func (h *Handler) profile(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name         string
		Avatar       string
		RemoveAvatar bool
	}
	limit := base64.StdEncoding.EncodedLen(auth.MaxAvatarBytes)
	if !accountInput(w, r, &input, int64(limit)+4096) {
		return
	}
	change := auth.ProfileChange{Name: input.Name}
	switch {
	case input.RemoveAvatar && input.Avatar != "":
		h.accountError(w, auth.ErrAvatar)
		return
	case input.RemoveAvatar:
		change.Avatar = &auth.Avatar{}
	case input.Avatar != "":
		image, err := base64.StdEncoding.DecodeString(input.Avatar)
		if err != nil || len(image) > auth.MaxAvatarBytes {
			h.accountError(w, auth.ErrAvatar)
			return
		}
		mediaType := http.DetectContentType(image)
		switch mediaType {
		case "image/png", "image/jpeg", "image/webp":
			change.Avatar = &auth.Avatar{MediaType: mediaType, Data: image}
		default:
			h.accountError(w, auth.ErrAvatar)
			return
		}
	}
	user, err := h.service.UpdateProfile(r.Context(), change)
	if err != nil {
		h.accountError(w, err)
		return
	}
	h.respond(w, &user)
}

func (h *Handler) avatar(w http.ResponseWriter, r *http.Request) {
	avatar, err := h.service.Avatar(r.Context(), r.PathValue("owner"), r.PathValue("version"))
	if err != nil {
		h.accountError(w, err)
		return
	}
	w.Header().Set("Content-Type", avatar.MediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if _, err := w.Write(avatar.Data); err != nil {
		h.logger.Debug("write account avatar", "error", err)
	}
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var input struct{ CurrentPassword, Password string }
	if !accountInput(w, r, &input, 4096) || !h.accountPasswordSlot(w) {
		return
	}
	defer func() { <-h.passwordSlots }()
	err := h.service.ChangePassword(r.Context(), readCookie(r, sessionCookie), input.CurrentPassword, input.Password)
	h.accountSession(w, r, err)
}

func (h *Handler) startEmailBinding(w http.ResponseWriter, r *http.Request) {
	var input struct{ Email, CurrentPassword string }
	if !accountInput(w, r, &input, 4096) || !h.accountPasswordSlot(w) {
		return
	}
	defer func() { <-h.passwordSlots }()
	id, err := h.service.SendEmailCode(r.Context(), h.config().Mailer, readCookie(r, sessionCookie), input.CurrentPassword, input.Email)
	if err != nil {
		h.accountError(w, err)
		return
	}
	h.writeJSON(w, struct {
		Request string `json:"request"`
	}{id})
}

func (h *Handler) verifyEmailBinding(w http.ResponseWriter, r *http.Request) {
	var input struct{ Request, Code string }
	if !accountInput(w, r, &input, 4096) {
		return
	}
	err := h.service.VerifyEmailCode(r.Context(), readCookie(r, sessionCookie), input.Request, input.Code)
	h.accountSession(w, r, err)
}

func (h *Handler) accountSession(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		h.accountError(w, err)
		return
	}
	h.session(w, r)
}

func (h *Handler) accounts(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	search, offset := query.Get("search"), 0
	if raw := query.Get("offset"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 31)
		if err != nil {
			authError(w, "AUTH_INVALID_REQUEST", "Invalid account list offset.", http.StatusBadRequest)
			return
		}
		offset = int(parsed)
	}
	if len(search) > 256 {
		authError(w, "AUTH_INVALID_REQUEST", "Account search is too long.", http.StatusBadRequest)
		return
	}
	page, err := h.service.ListAccounts(r.Context(), search, offset, adminPageSize)
	if err != nil {
		h.accountError(w, err)
		return
	}
	users := make([]userResponse, 0, len(page.Users))
	for _, user := range page.Users {
		users = append(users, publicUser(user))
	}
	h.writeJSON(w, struct {
		Users []userResponse `json:"users"`
		Total int64          `json:"total"`
		Limit int            `json:"limit"`
	}{users, page.Total, adminPageSize})
}

func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	if !auth.CurrentUser(r.Context()).Admin {
		h.accountError(w, auth.ErrAdminRequired)
		return
	}
	var input struct{ Email, Password, Name string }
	if !accountInput(w, r, &input, 4096) || !h.accountPasswordSlot(w) {
		return
	}
	defer func() { <-h.passwordSlots }()
	user, err := h.service.CreateManagedAccount(r.Context(), input.Email, input.Password, input.Name)
	if err != nil {
		h.accountError(w, err)
		return
	}
	h.writeJSON(w, publicUser(user))
}

func (h *Handler) changeAccess(w http.ResponseWriter, r *http.Request) {
	var input auth.AccountAccess
	if !accountInput(w, r, &input, 4096) {
		return
	}
	if (input.Admin == nil) == (input.Disabled == nil) {
		authError(w, "AUTH_INVALID_REQUEST", "Change one account access setting at a time.", http.StatusBadRequest)
		return
	}
	user, err := h.service.SetAccountAccess(r.Context(), r.PathValue("id"), input)
	if err != nil {
		h.accountError(w, err)
		return
	}
	h.writeJSON(w, publicUser(user))
}

func (h *Handler) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if !accountInput(w, r, &struct{}{}, 4096) {
		return
	}
	target := r.PathValue("id")
	if err := h.service.DeleteAccount(r.Context(), target); err != nil {
		h.accountError(w, err)
		return
	}
	if after := h.config().AccountDeleted; after != nil {
		after(target)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) accountPasswordSlot(w http.ResponseWriter) bool {
	if !h.accountRate.Allow() {
		authError(w, "AUTH_RATE_LIMITED", "Too many account requests. Try again shortly.", http.StatusTooManyRequests)
		return false
	}
	select {
	case h.passwordSlots <- struct{}{}:
		return true
	default:
		authError(w, "AUTH_BUSY", "Authentication is busy. Try again shortly.", http.StatusTooManyRequests)
		return false
	}
}

func accountInput(w http.ResponseWriter, r *http.Request, input any, size int64) bool {
	decoder := json.NewDecoder(httpserver.MaxBytesReader(w, r.Body, size))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(input); err == nil && errors.Is(decoder.Decode(new(any)), io.EOF) {
		return true
	}
	authError(w, "AUTH_INVALID_REQUEST", "Invalid account request.", http.StatusBadRequest)
	return false
}

func (h *Handler) accountError(w http.ResponseWriter, err error) {
	var code string
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, auth.ErrName):
		code = "ACCOUNT_INVALID_NAME"
	case errors.Is(err, auth.ErrAvatar):
		code = "ACCOUNT_INVALID_AVATAR"
	case errors.Is(err, auth.ErrEmail):
		code = "ACCOUNT_INVALID_EMAIL"
	case errors.Is(err, auth.ErrPassword):
		code = "ACCOUNT_INVALID_PASSWORD"
	case errors.Is(err, auth.ErrCurrent):
		code, status = "ACCOUNT_PASSWORD_INCORRECT", http.StatusForbidden
	case errors.Is(err, auth.ErrRecentSignIn):
		code, status = "ACCOUNT_RECENT_SIGN_IN", http.StatusForbidden
	case errors.Is(err, auth.ErrAdminRequired):
		code, status = "ADMIN_REQUIRED", http.StatusForbidden
	case errors.Is(err, auth.ErrAdminSelf):
		code, status = "ACCOUNT_ADMIN_SELF", http.StatusForbidden
	case errors.Is(err, auth.ErrAccountMissing):
		code, status = "ACCOUNT_NOT_FOUND", http.StatusNotFound
	case errors.Is(err, auth.ErrOAuthDisabled):
		code, status = "ACCOUNT_OAUTH_DISABLED", http.StatusServiceUnavailable
	case errors.Is(err, auth.ErrOAuthLinked):
		code, status = "ACCOUNT_OAUTH_LINKED", http.StatusConflict
	case errors.Is(err, auth.ErrOAuthPassword):
		code, status = "ACCOUNT_OAUTH_NEEDS_PASSWORD", http.StatusConflict
	case errors.Is(err, auth.ErrBindEmail):
		code = "ACCOUNT_BIND_EMAIL"
	case errors.Is(err, auth.ErrEmailInUse):
		code, status = "ACCOUNT_EMAIL_IN_USE", http.StatusConflict
	case errors.Is(err, auth.ErrEmailUnchanged):
		code = "ACCOUNT_EMAIL_UNCHANGED"
	case errors.Is(err, auth.ErrEmailCode):
		code = "ACCOUNT_EMAIL_CODE"
	case errors.Is(err, auth.ErrEmailWait):
		code, status = "ACCOUNT_EMAIL_WAIT", http.StatusTooManyRequests
	case errors.Is(err, auth.ErrEmailDisabled):
		code, status = "ACCOUNT_EMAIL_DISABLED", http.StatusServiceUnavailable
	case errors.Is(err, auth.ErrEmailDelivery):
		h.logger.Warn("verification email delivery failed", "error", err)
		authError(w, "ACCOUNT_EMAIL_DELIVERY", auth.ErrEmailDelivery.Error(), http.StatusBadGateway)
		return
	case errors.Is(err, auth.ErrAccountChanged):
		code, status = "ACCOUNT_CHANGED", http.StatusConflict
	default:
		h.failure(w, err)
		return
	}
	authError(w, code, err.Error(), status)
}
