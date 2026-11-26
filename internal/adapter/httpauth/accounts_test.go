package httpauth_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/adapter/httpauth"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
)

type verificationMail struct{ auth.EmailVerification }

func (m *verificationMail) SendVerification(_ context.Context, verification auth.EmailVerification) error {
	m.EmailVerification = verification
	return nil
}

type testHasher struct{}

func (testHasher) Hash(value string) []byte             { return []byte(value) }
func (testHasher) Match(value string, hash []byte) bool { return value == string(hash) }

type blockingHasher struct {
	testHasher
	started, resume chan struct{}
	waited          atomic.Bool
}

func (h *blockingHasher) Match(value string, hash []byte) bool {
	if h.waited.CompareAndSwap(false, true) {
		close(h.started)
		<-h.resume
	}
	return h.testHasher.Match(value, hash)
}

func accountFixture(t *testing.T, mailer auth.VerificationMailer) (*sqlite.Database, *auth.Service, *http.ServeMux) {
	t.Helper()
	repo, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	service := auth.New(repo.Accounts, testHasher{}, uuid.NewString)
	mux := http.NewServeMux()
	handler := httpauth.NewWithConfig(service, func() httpauth.Config {
		return httpauth.Config{Mailer: mailer}
	}, httpauth.DefaultLimits(), slog.New(slog.DiscardHandler))
	handler.Routes(mux, http.NewCrossOriginProtection().Handler)
	return repo, service, mux
}

func accountCookie(t *testing.T, service *auth.Service, user auth.User) *http.Cookie {
	t.Helper()
	token, err := service.StartSession(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "shinona_session", Value: token}
}

func accountResponse(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	assertStatus(t, response, http.StatusOK)
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func assertAccountError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	assertStatus(t, response, status)
	if got := response.Header().Get("X-Shinona-Error-Code"); got != code {
		t.Fatalf("error code %q, expected %q", got, code)
	}
}

func TestOAuthAccountCanBindEmailAndSetPassword(t *testing.T) {
	mail := &verificationMail{}
	_, service, mux := accountFixture(t, mail)
	identity := auth.Identity{Issuer: "https://idp.example", Subject: "example", Email: "sso@example.com", Name: "SSO Name"}
	user, err := service.OAuthUser(t.Context(), identity)
	if err != nil {
		t.Fatal(err)
	}
	current := accountCookie(t, service, user)
	other := accountCookie(t, service, user)
	const email = "member@example.com"
	const password = "a-good-test-password"
	assertAccountError(t, perform(t, mux, "POST", "/auth/account/password", `{"password":"`+password+`"}`, current),
		http.StatusBadRequest, "ACCOUNT_BIND_EMAIL")
	before := time.Now().Add(auth.EmailLifetime)
	sent := accountResponse(t, perform(t, mux, "POST", "/auth/account/email/start", `{"email":"MEMBER@example.com"}`, current))
	if got := sent["request"]; got != mail.RequestID || mail.Address != email || len(mail.Code) != 8 || sent["code"] != nil {
		t.Fatalf("unexpected verification result: %#v", sent)
	}
	if mail.ExpiresAt.Before(before) || mail.ExpiresAt.After(time.Now().Add(auth.EmailLifetime)) {
		t.Fatalf("unexpected verification expiry %v", mail.ExpiresAt)
	}
	assertAccountError(t, perform(t, mux, "POST", "/auth/account/email/start", `{"email":"other@example.com"}`, current),
		http.StatusTooManyRequests, "ACCOUNT_EMAIL_WAIT")
	verify := `{"request":"` + mail.RequestID + `","code":"` + mail.Code + `"}`
	assertAccountError(t, perform(t, mux, "POST", "/auth/account/email/verify", verify, other),
		http.StatusBadRequest, "ACCOUNT_EMAIL_CODE")
	wrongCode := "00000000"
	if wrongCode == mail.Code {
		wrongCode = "00000001"
	}
	assertAccountError(t, perform(t, mux, "POST", "/auth/account/email/verify", `{"request":"`+mail.RequestID+`","code":"`+wrongCode+`"}`, current),
		http.StatusBadRequest, "ACCOUNT_EMAIL_CODE")
	assertStatus(t, perform(t, mux, "POST", "/auth/account/email/verify", verify, current), http.StatusOK)
	assertAccountError(t, perform(t, mux, "POST", "/auth/account/email/verify", verify, current),
		http.StatusBadRequest, "ACCOUNT_EMAIL_CODE")
	assertStatus(t, perform(t, mux, "GET", "/auth/admin/users", "", other), http.StatusUnauthorized)
	assertStatus(t, perform(t, mux, "POST", "/auth/account/password", `{"password":"`+password+`"}`, current), http.StatusOK)
	login := perform(t, mux, "POST", "/auth/password", `{"email":"`+email+`","password":"`+password+`"}`)
	assertStatus(t, login, http.StatusOK)
	assertAvatar(t, mux, login.Result().Cookies()[0], user.ID, "")
	linked, err := service.OAuthUser(t.Context(), identity)
	if err != nil || linked.ID != user.ID || linked.LoginEmail != email {
		t.Fatalf("OAuth account changed: %+v, %v", linked, err)
	}
}

func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	_, service, mux := accountFixture(t, nil)
	const email = "local@example.com"
	const password = "a-good-test-password"
	user, err := service.CreatePasswordUser(t.Context(), email, password)
	if err != nil {
		t.Fatal(err)
	}
	current, other := accountCookie(t, service, user), accountCookie(t, service, user)
	assertAccountError(t, perform(t, mux, "POST", "/auth/account/password", `{"currentPassword":"bad","password":"new-password-42"}`, current),
		http.StatusForbidden, "ACCOUNT_PASSWORD_INCORRECT")
	assertStatus(t, perform(t, mux, "POST", "/auth/account/password", `{"currentPassword":"`+password+`","password":"new-password-42"}`, current), http.StatusOK)
	assertStatus(t, perform(t, mux, "POST", "/auth/account/profile", `{"name":"Revoked"}`, other), http.StatusUnauthorized)
	assertStatus(t, perform(t, mux, "POST", "/auth/password", `{"email":"`+email+`","password":"`+password+`"}`), http.StatusUnauthorized)
	assertStatus(t, perform(t, mux, "POST", "/auth/password", `{"email":"`+email+`","password":"new-password-42"}`), http.StatusOK)
	assertAvatar(t, mux, current, user.ID, "")
}

func TestAccountPasswordRateLimitsAreIndependent(t *testing.T) {
	_, service, mux := accountFixture(t, nil)
	const password = "a-good-test-password"
	cookie := func(email string) *http.Cookie {
		t.Helper()
		user, err := service.CreatePasswordUser(t.Context(), email, password)
		if err != nil {
			t.Fatal(err)
		}
		return accountCookie(t, service, user)
	}
	first, second := cookie("first@example.com"), cookie("second@example.com")
	path := "/auth/account/password"
	body := `{"currentPassword":"wrong","password":"new-password-42"}`
	for range 5 {
		assertAccountError(t, perform(t, mux, http.MethodPost, path, body, first), http.StatusForbidden, "ACCOUNT_PASSWORD_INCORRECT")
	}
	assertAccountError(t, perform(t, mux, http.MethodPost, path, body, first), http.StatusTooManyRequests, "AUTH_RATE_LIMITED")
	assertAccountError(t, perform(t, mux, http.MethodPost, path, body, second), http.StatusForbidden, "ACCOUNT_PASSWORD_INCORRECT")
}

func TestPasswordConcurrencyIsSharedAndReleased(t *testing.T) {
	repo, setup, _ := accountFixture(t, nil)
	const email, password = "member@example.com", "a-good-test-password"
	user, err := setup.CreatePasswordUser(t.Context(), email, password)
	if err != nil {
		t.Fatal(err)
	}
	cookie := accountCookie(t, setup, user)
	hasher := &blockingHasher{started: make(chan struct{}), resume: make(chan struct{})}
	resume := sync.OnceFunc(func() { close(hasher.resume) })
	limits := httpauth.DefaultLimits()
	limits.PasswordConcurrency = 1
	service := auth.New(repo.Accounts, hasher, uuid.NewString)
	handler := httpauth.NewWithConfig(service, func() httpauth.Config { return httpauth.Config{} },
		limits, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	handler.Routes(mux, http.NewCrossOriginProtection().Handler)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/auth/password",
		bytes.NewBufferString(`{"email":"`+email+`","password":"`+password+`"}`))
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		mux.ServeHTTP(response, request)
	}()
	defer func() {
		resume()
		<-done
	}()
	select {
	case <-hasher.started:
	case <-done:
		t.Fatalf("login ended without starting verification: %d", response.Code)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	path := "/auth/account/password"
	body := `{"currentPassword":"wrong","password":"new-password-42"}`
	assertAccountError(t, perform(t, mux, http.MethodPost, path, body, cookie), http.StatusTooManyRequests, "AUTH_BUSY")

	resume()
	select {
	case <-done:
		assertStatus(t, response, http.StatusOK)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assertAccountError(t, perform(t, mux, http.MethodPost, path, body, cookie), http.StatusForbidden, "ACCOUNT_PASSWORD_INCORRECT")
}

func TestAccountProfileAndAdminAccess(t *testing.T) {
	repo, service, mux := accountFixture(t, nil)
	member, err := service.OAuthUser(t.Context(), auth.Identity{
		Issuer: "https://idp.example", Subject: "member", Email: "member@example.com", AvatarURL: tokenAvatar,
	})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := service.CreatePasswordUser(t.Context(), "admin@example.com", "admin-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Accounts.SetAdmin(t.Context(), admin.ID, true); err != nil {
		t.Fatal(err)
	}
	memberCookie, adminCookie := accountCookie(t, service, member), accountCookie(t, service, admin)
	assertAccountError(t, perform(t, mux, "GET", "/auth/admin/users", "", memberCookie), http.StatusForbidden, "ADMIN_REQUIRED")
	assertAccountError(t, perform(t, mux, "POST", "/auth/admin/users/"+admin.ID+"/access", `{"disabled":true}`, memberCookie),
		http.StatusForbidden, "ADMIN_REQUIRED")
	assertAccountError(t, perform(t, mux, "POST", "/auth/admin/users/"+admin.ID+"/access", `{"disabled":true}`, adminCookie),
		http.StatusForbidden, "ACCOUNT_ADMIN_SELF")
	created := accountResponse(t, perform(t, mux, "POST", "/auth/admin/users",
		`{"email":"created@example.com","name":"Created user","password":"created-password-42"}`, adminCookie))
	if created["name"] != "Created user" || created["admin"] != false || created["hasPassword"] != true {
		t.Fatalf("unexpected created user: %+v", created)
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"  New Name  ","avatar":"` + base64.StdEncoding.EncodeToString(pngData.Bytes()) + `"}`
	payload := accountResponse(t, perform(t, mux, "POST", "/auth/account/profile", body, memberCookie))
	profile, ok := payload["user"].(map[string]any)
	if !ok || profile["name"] != "New Name" || profile["hasAvatar"] != true {
		t.Fatalf("profile did not update: %+v", payload)
	}
	avatar, ok := profile["avatarUrl"].(string)
	if !ok || avatar == "" {
		t.Fatalf("missing avatar URL: %+v", profile)
	}
	imageResponse := perform(t, mux, "GET", avatar, "", adminCookie)
	assertStatus(t, imageResponse, http.StatusOK)
	if imageResponse.Header().Get("Content-Type") != "image/png" || !bytes.Equal(imageResponse.Body.Bytes(), pngData.Bytes()) {
		t.Fatal("avatar response does not match upload")
	}
	list := accountResponse(t, perform(t, mux, "GET", "/auth/admin/users?search=New+Name", "", adminCookie))
	if list["total"] != float64(1) {
		t.Fatalf("unexpected filtered list: %+v", list)
	}
	literal := accountResponse(t, perform(t, mux, "GET", "/auth/admin/users?search=%25", "", adminCookie))
	if literal["total"] != float64(0) {
		t.Fatal("search interpreted % as a wildcard")
	}
	accessPath := "/auth/admin/users/" + member.ID + "/access"
	assertStatus(t, perform(t, mux, "POST", accessPath, `{"admin":true}`, adminCookie), http.StatusOK)
	assertStatus(t, perform(t, mux, "GET", "/auth/admin/users", "", memberCookie), http.StatusOK)
	assertStatus(t, perform(t, mux, "POST", accessPath, `{"admin":false}`, adminCookie), http.StatusOK)
	assertAccountError(t, perform(t, mux, "GET", "/auth/admin/users", "", memberCookie), http.StatusForbidden, "ADMIN_REQUIRED")
	assertStatus(t, perform(t, mux, "POST", accessPath, `{"disabled":true}`, adminCookie), http.StatusOK)
	assertStatus(t, perform(t, mux, "POST", "/auth/account/profile", body, memberCookie), http.StatusUnauthorized)
	identity := auth.Identity{Issuer: "https://idp.example", Subject: "member", Name: "Original", AvatarURL: userInfoAvatar}
	if _, err := service.OAuthUser(t.Context(), identity); err == nil {
		t.Fatal("disabled user could log in via OAuth")
	}
	assertStatus(t, perform(t, mux, "POST", accessPath, `{"disabled":false}`, adminCookie), http.StatusOK)
	restored, err := service.OAuthUser(t.Context(), identity)
	if err != nil || restored.Name != "New Name" {
		t.Fatalf("custom profile did not survive OAuth login: %+v, %v", restored, err)
	}
	restoredCookie := accountCookie(t, service, restored)
	assertAvatar(t, mux, restoredCookie, member.ID, avatar)
	assertStatus(t, perform(t, mux, "POST", "/auth/account/profile", `{"name":"New Name","removeAvatar":true}`, restoredCookie), http.StatusOK)
	assertAvatar(t, mux, restoredCookie, member.ID, userInfoAvatar)
	assertStatus(t, perform(t, mux, "GET", avatar, "", adminCookie), http.StatusNotFound)
}
