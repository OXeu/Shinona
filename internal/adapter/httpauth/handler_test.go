package httpauth_test

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/OXeu/shinona/internal/adapter/httpauth"
	"github.com/OXeu/shinona/internal/adapter/oidc"
	"github.com/OXeu/shinona/internal/adapter/password"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
)

const tokenAvatar = "https://images.example.test/token.webp"
const userInfoAvatar = "https://images.example.test/userinfo.webp"

func TestPasswordAndOAuthSignIn(t *testing.T) {
	var signingEnabled atomic.Bool
	issuer, authorization := oidcFixture(t, &signingEnabled)
	provider, err := oidc.NewWithClient(t.Context(), issuer, "test-client", "test-secret", "https://shinona.example/auth/oauth/callback", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	service := auth.New(repo, password.Argon2id{}, uuid.NewString)
	user, err := service.CreatePasswordUser(t.Context(), "reviewer@example.test", "example-password-42")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	handler := httpauth.New(service, provider, true, slog.New(slog.DiscardHandler))
	handler.Routes(mux, http.NewCrossOriginProtection().Handler)
	mux.Handle("POST /test/protected", handler.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, auth.CurrentUser(r.Context()).ID)
	})))

	passwordBody := `{"email":"reviewer@example.test","password":"example-password-42"}`
	assertStatus(t, perform(t, mux, "POST", "/test/protected", "{}"), http.StatusUnauthorized)
	wrongPassword := perform(t, mux, "POST", "/auth/password", `{"email":"reviewer@example.test","password":"wrong"}`)
	assertStatus(t, wrongPassword, http.StatusUnauthorized)
	if wrongPassword.Header().Get("X-Shinona-Error-Code") != "AUTH_CREDENTIALS" {
		t.Fatal("missing credential error code")
	}
	login := perform(t, mux, "POST", "/auth/password", passwordBody)
	assertStatus(t, login, http.StatusOK)
	cookie := login.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected session cookie: %v", cookie)
	}
	protected := perform(t, mux, "POST", "/test/protected", "{}", cookie)
	assertStatus(t, protected, http.StatusOK)
	if protected.Body.String() != user.ID {
		t.Fatal("password session resolved to a different account")
	}
	assertStatus(t, perform(t, mux, "POST", "/auth/logout", "{}", cookie), http.StatusNoContent)
	assertStatus(t, perform(t, mux, "POST", "/test/protected", "{}", cookie), http.StatusUnauthorized)

	oauthStart := startOAuth(t, mux, authorization)
	callback := "/auth/oauth/callback?state=" + url.QueryEscape(oauthStart.Value)
	missingKeys := perform(t, mux, "GET", callback+"&code=good-code", "", oauthStart)
	if missingKeys.Header().Get("Location") != "/login?oauth=failed" {
		t.Fatal("OAuth accepted a token without a published signing key")
	}

	signingEnabled.Store(true)
	oauthStart = startOAuth(t, mux, authorization)
	callback = "/auth/oauth/callback?state=" + url.QueryEscape(oauthStart.Value)
	invalid := perform(t, mux, "GET", callback+"&code=bad-nonce", "", oauthStart)
	if invalid.Header().Get("Location") != "/login?oauth=failed" {
		t.Fatal("OAuth accepted an invalid nonce")
	}

	oauthStart = startOAuth(t, mux, authorization)
	callback = "/auth/oauth/callback?state=" + url.QueryEscape(oauthStart.Value) + "&code=good-code"
	loggedIn := perform(t, mux, "GET", callback, "", oauthStart)
	assertStatus(t, loggedIn, http.StatusSeeOther)
	if loggedIn.Header().Get("Location") != "/" {
		t.Fatalf("OAuth callback redirected to %q", loggedIn.Header().Get("Location"))
	}
	var oauthSession *http.Cookie
	for _, resultCookie := range loggedIn.Result().Cookies() {
		if resultCookie.Name == cookie.Name {
			oauthSession = resultCookie
		}
	}
	if oauthSession == nil {
		t.Fatal("OAuth login did not create a session")
	}
	protected = perform(t, mux, "POST", "/test/protected", "{}", oauthSession)
	assertStatus(t, protected, http.StatusOK)
	if protected.Body.String() == user.ID {
		t.Fatal("OAuth must not attach to an existing password account by email")
	}
	oauthUserID := protected.Body.String()
	assertAvatar(t, mux, oauthSession, oauthUserID, tokenAvatar)
	replay := perform(t, mux, "GET", callback, "", oauthStart)
	if replay.Header().Get("Location") != "/login?oauth=failed" {
		t.Fatal("OAuth callback accepted replayed state")
	}
	for _, attempt := range []struct{ code, avatar string }{
		{"userinfo-code", userInfoAvatar},
		{"wrong-subject-code", ""},
	} {
		start := startOAuth(t, mux, authorization)
		path := "/auth/oauth/callback?state=" + url.QueryEscape(start.Value) + "&code=" + attempt.code
		updated := perform(t, mux, "GET", path, "", start)
		if updated.Header().Get("Location") != "/" {
			t.Fatalf("OAuth sign-in failed with %s", attempt.code)
		}
		assertAvatar(t, mux, oauthSession, oauthUserID, attempt.avatar)
	}
}

func assertAvatar(t *testing.T, mux *http.ServeMux, cookie *http.Cookie, id, avatar string) {
	t.Helper()
	session := perform(t, mux, "GET", "/auth/session", "", cookie)
	assertStatus(t, session, http.StatusOK)
	var result struct {
		User struct {
			ID        string `json:"id"`
			AvatarURL string `json:"avatarUrl"`
		} `json:"user"`
	}
	if err := json.NewDecoder(session.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.User.ID != id || result.User.AvatarURL != avatar {
		t.Fatalf("session has user %q and avatar %q, expected user %q and avatar %q", result.User.ID, result.User.AvatarURL, id, avatar)
	}
}

type challenge struct {
	nonce string
	pkce  string
}

func startOAuth(t *testing.T, mux *http.ServeMux, authorization *atomic.Value) *http.Cookie {
	t.Helper()
	started := perform(t, mux, "GET", "/auth/oauth/start", "")
	assertStatus(t, started, http.StatusSeeOther)
	destination, err := url.Parse(started.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	parameters := destination.Query()
	if !slices.Contains(strings.Fields(parameters.Get("scope")), "profile") {
		t.Fatal("OAuth authorization is missing the profile scope")
	}
	cookie := started.Result().Cookies()[0]
	if parameters.Get("state") != cookie.Value || parameters.Get("nonce") == "" ||
		parameters.Get("code_challenge_method") != "S256" || parameters.Get("code_challenge") == "" {
		t.Fatalf("OAuth authorization is missing state, nonce, or PKCE: %v", destination)
	}
	authorization.Store(challenge{parameters.Get("nonce"), parameters.Get("code_challenge")})
	return cookie
}

func oidcFixture(t *testing.T, signingEnabled *atomic.Bool) (string, *atomic.Value) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var required atomic.Value
	required.Store(challenge{})
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		metadata := map[string]any{
			"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token",
			"userinfo_endpoint": server.URL + "/userinfo",
		}
		if signingEnabled.Load() {
			metadata["jwks_uri"] = server.URL + "/keys"
			metadata["id_token_signing_alg_values_supported"] = []string{"RS256"}
		}
		sendJSON(t, w, metadata)
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		sendJSON(t, w, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test-key",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		subject := "oauth-subject"
		if r.Header.Get("Authorization") == "Bearer wrong-subject-code" {
			subject = "different-subject"
		}
		if r.Header.Get("Authorization") == "Bearer good-code" {
			t.Error("unexpected UserInfo request when the ID token supplies an avatar")
		}
		sendJSON(t, w, map[string]string{"sub": subject, "picture": userInfoAvatar})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token request: %v", err)
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		expected, _ := required.Load().(challenge)
		if oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != expected.pkce {
			t.Error("incorrect OAuth verifier")
		}
		nonce := expected.nonce
		code := r.Form.Get("code")
		if code == "bad-nonce" {
			nonce = "incorrect"
		}
		avatar := ""
		if code == "good-code" {
			avatar = tokenAvatar
		}
		token, err := signedToken(key, server.URL, nonce, avatar)
		if err != nil {
			t.Errorf("sign ID token: %v", err)
			http.Error(w, "signing failed", http.StatusInternalServerError)
			return
		}
		sendJSON(t, w, map[string]any{"access_token": code, "token_type": "Bearer", "id_token": token})
	})
	return server.URL, &required
}

func signedToken(key *rsa.PrivateKey, issuer, nonce, avatar string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"iss": issuer, "sub": "oauth-subject", "aud": "test-client", "exp": 4_000_000_000, "nonce": nonce,
		"email": "reviewer@example.test", "email_verified": true, "picture": avatar,
	})
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test-key"}`))
	input := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), err
}

func sendJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func perform(t *testing.T, handler http.Handler, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	return result
}

func assertStatus(t *testing.T, response *httptest.ResponseRecorder, expected int) {
	t.Helper()
	if response.Code != expected {
		t.Fatalf("status %d, expected %d (%s)", response.Code, expected, strings.TrimSpace(response.Body.String()))
	}
}
