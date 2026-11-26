package httpauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"

	"github.com/OXeu/shinona/internal/adapter/httpauth"
	"github.com/OXeu/shinona/internal/auth"
)

const accountIssuer = "https://account-issuer.example"

type accountProvider struct{}

func (accountProvider) AuthorizationURL(state string, _ auth.Flow) string {
	return accountIssuer + "/authorize?state=" + url.QueryEscape(state)
}

func (accountProvider) Exchange(_ context.Context, code string, _ auth.Flow) (auth.Identity, error) {
	return auth.Identity{Issuer: accountIssuer, Subject: code, Email: "provider@example.test", AvatarURL: tokenAvatar}, nil
}

func oauthAccountHandler(service *auth.Service) *http.ServeMux {
	handler := httpauth.New(service, accountProvider{}, true, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	handler.Routes(mux, http.NewCrossOriginProtection().Handler)
	return mux
}

func beginAccountLink(t *testing.T, mux http.Handler, cookie *http.Cookie, password string) (string, *http.Cookie) {
	t.Helper()
	link := perform(t, mux, "POST", "/auth/account/oauth/link", `{"currentPassword":"`+password+`"}`, cookie)
	data := accountResponse(t, link)
	raw, ok := data["url"].(string)
	if !ok {
		t.Fatalf("missing authorization URL: %#v", data)
	}
	target, err := url.Parse(raw)
	if err != nil || target.Query().Get("state") == "" {
		t.Fatalf("invalid authorization URL: %q", raw)
	}
	cookies := link.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "shinona_oauth_flow" || cookies[0].Path != "/auth" ||
		!cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatal("missing protected link-flow cookie")
	}
	return target.Query().Get("state"), cookies[0]
}

func TestAccountOAuthRouteReplacesPreviousBrowserFlow(t *testing.T) {
	_, service, _ := accountFixture(t, nil)
	mux := oauthAccountHandler(service)
	const password = "member-password-42"
	user, err := service.CreatePasswordUser(t.Context(), "existing@example.test", password)
	if err != nil {
		t.Fatal(err)
	}
	current := accountCookie(t, service, user)
	previous, cookie := beginAccountLink(t, mux, current, password)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/auth/account/oauth/link", "/auth/oauth/link"} {
		target := &url.URL{Scheme: "https", Host: "shinona.example.test", Path: path}
		jar.SetCookies(target, []*http.Cookie{cookie})
		cookies := append(jar.Cookies(target), current)
		response := perform(t, mux, "POST", path, `{"currentPassword":"`+password+`"}`, cookies...)
		assertStatus(t, response, http.StatusOK)
		if flow, err := service.ConsumeFlow(t.Context(), previous); err != nil || flow != nil {
			t.Fatalf("%s did not cancel the superseded OAuth flow: %v", path, err)
		}
		cookie = response.Result().Cookies()[0]
		previous = cookie.Value[len("link:"):]
	}
}

func TestOAuthCallbackAcceptsLegacyFlowCookie(t *testing.T) {
	_, service, _ := accountFixture(t, nil)
	mux := oauthAccountHandler(service)
	state, _, err := service.BeginFlow(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	old := &http.Cookie{Name: "shinona_oauth", Value: state, Path: "/auth/oauth"}
	response := perform(t, mux, "GET", "/auth/oauth/callback?code=legacy&state="+url.QueryEscape(state), "", old)
	if got := response.Header().Get("Location"); got != "/" {
		t.Fatalf("callback for a legacy OAuth cookie redirected to %q", got)
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == old.Name && cookie.Path == old.Path && cookie.MaxAge < 0 {
			return
		}
	}
	t.Fatal("legacy OAuth flow cookie was not cleared")
}

func TestLinkAndUnlinkOAuthOnExistingAccount(t *testing.T) {
	_, service, _ := accountFixture(t, nil)
	mux := oauthAccountHandler(service)
	const password = "member-password-42"
	user, err := service.CreatePasswordUser(t.Context(), "existing@example.test", password)
	if err != nil {
		t.Fatal(err)
	}
	current, other := accountCookie(t, service, user), accountCookie(t, service, user)
	if data := accountResponse(t, perform(t, mux, "GET", "/auth/account/oauth", "", current)); data["linked"] != false {
		t.Fatal("password account starts linked")
	}
	assertAccountError(t, perform(t, mux, "POST", "/auth/oauth/link", `{"currentPassword":"wrong"}`, current),
		http.StatusForbidden, "ACCOUNT_PASSWORD_INCORRECT")
	state, flowCookie := beginAccountLink(t, mux, current, password)
	callback := "/auth/oauth/callback?code=subject-linked&state=" + url.QueryEscape(state)
	link := perform(t, mux, "GET", callback, "", flowCookie, current)
	assertStatus(t, link, http.StatusSeeOther)
	if link.Header().Get("Location") != "/account?oauthLink=linked" {
		t.Fatalf("link callback returned %s", link.Header().Get("Location"))
	}
	if data := accountResponse(t, perform(t, mux, "GET", "/auth/account/oauth", "", current)); data["linked"] != true {
		t.Fatal("identity was not linked")
	}
	assertAvatar(t, mux, current, user.ID, tokenAvatar)
	replay := perform(t, mux, "GET", callback, "", flowCookie, current)
	if replay.Header().Get("Location") == "/account?oauthLink=linked" {
		t.Fatal("link code was replayed")
	}

	signIn := perform(t, mux, "GET", "/auth/oauth/start", "")
	destination, err := url.Parse(signIn.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	signedIn := perform(t, mux, "GET", "/auth/oauth/callback?code=subject-linked&state="+
		url.QueryEscape(destination.Query().Get("state")), "", signIn.Result().Cookies()[0])
	if signedIn.Header().Get("Location") != "/" {
		t.Fatal("linked identity could not sign in")
	}
	var oauthSession *http.Cookie
	for _, cookie := range signedIn.Result().Cookies() {
		if cookie.Name == "shinona_session" {
			oauthSession = cookie
		}
	}
	if oauthSession == nil {
		t.Fatal("OAuth sign-in did not set a session")
	}
	assertAvatar(t, mux, oauthSession, user.ID, tokenAvatar)
	assertStatus(t, perform(t, mux, "POST", "/auth/account/oauth/unlink", `{"currentPassword":"`+password+`"}`, current), http.StatusOK)
	assertAvatar(t, mux, current, user.ID, "")
	for _, revoked := range []*http.Cookie{other, oauthSession} {
		assertStatus(t, perform(t, mux, "GET", "/auth/account/oauth", "", revoked), http.StatusUnauthorized)
	}
	if _, err := service.StartOAuthSession(t.Context(), user, auth.Identity{
		Issuer: accountIssuer, Subject: "subject-linked",
	}); !errors.Is(err, auth.ErrCredentials) {
		t.Fatalf("unlinked OAuth credential created a session: %v", err)
	}
}

func TestOAuthLinkDisallowsWrongSessionAndExistingIdentity(t *testing.T) {
	_, service, _ := accountFixture(t, nil)
	mux := oauthAccountHandler(service)
	const password = "member-password-42"
	user, err := service.CreatePasswordUser(t.Context(), "existing@example.test", password)
	if err != nil {
		t.Fatal(err)
	}
	alreadyLinked, err := service.OAuthUser(t.Context(), auth.Identity{Issuer: accountIssuer, Subject: "other-account"})
	if err != nil {
		t.Fatal(err)
	}
	current, other := accountCookie(t, service, user), accountCookie(t, service, alreadyLinked)
	assertAccountError(t, perform(t, mux, "POST", "/auth/account/oauth/unlink", `{}`, other),
		http.StatusConflict, "ACCOUNT_OAUTH_NEEDS_PASSWORD")

	state, flowCookie := beginAccountLink(t, mux, current, password)
	callback := "/auth/oauth/callback?code=unused&state=" + url.QueryEscape(state)
	if got := perform(t, mux, "GET", callback, "", flowCookie, other).Header().Get("Location"); got != "/account?oauthLink=failed" {
		t.Fatalf("binding used a different login session: %s", got)
	}
	state, flowCookie = beginAccountLink(t, mux, current, password)
	callback = "/auth/oauth/callback?code=other-account&state=" + url.QueryEscape(state)
	if got := perform(t, mux, "GET", callback, "", flowCookie, current).Header().Get("Location"); got != "/account?oauthLink=in-use" {
		t.Fatalf("an existing binding was not protected: %s", got)
	}
	for _, account := range []struct {
		cookie *http.Cookie
		linked bool
	}{{current, false}, {other, true}} {
		if data := accountResponse(t, perform(t, mux, "GET", "/auth/account/oauth", "", account.cookie)); data["linked"] != account.linked {
			t.Fatalf("OAuth binding changed: %#v", data)
		}
	}
}

func TestDeleteAccountRequiresAdminAndNotifiesOnSuccess(t *testing.T) {
	repo, service, _ := accountFixture(t, nil)
	admin, err := service.CreatePasswordUser(t.Context(), "admin@example.test", "member-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Accounts.SetAdmin(t.Context(), admin.ID, true); err != nil {
		t.Fatal(err)
	}
	const email = "deleted@example.test"
	member, err := service.CreatePasswordUser(t.Context(), email, "member-password-42")
	if err != nil {
		t.Fatal(err)
	}
	var deleted string
	handler := httpauth.NewWithConfig(service, func() httpauth.Config {
		return httpauth.Config{AccountDeleted: func(owner string) { deleted = owner }}
	}, httpauth.DefaultLimits(), slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	handler.Routes(mux, http.NewCrossOriginProtection().Handler)
	adminCookie, memberCookie := accountCookie(t, service, admin), accountCookie(t, service, member)
	path := "/auth/admin/users/" + member.ID + "/delete"
	assertAccountError(t, perform(t, mux, "POST", path, "{}", memberCookie), http.StatusForbidden, "ADMIN_REQUIRED")
	assertAccountError(t, perform(t, mux, "POST", "/auth/admin/users/"+admin.ID+"/delete", "{}", adminCookie),
		http.StatusForbidden, "ACCOUNT_ADMIN_SELF")
	if deleted != "" {
		t.Fatal("rejected deletion called the hook")
	}
	assertStatus(t, perform(t, mux, "POST", path, "{}", adminCookie), http.StatusNoContent)
	if deleted != member.ID {
		t.Fatal("successful deletion did not notify the account lifecycle hook")
	}
	assertAccountError(t, perform(t, mux, "POST", path, "{}", adminCookie), http.StatusNotFound, "ACCOUNT_NOT_FOUND")
	assertStatus(t, perform(t, mux, "GET", "/auth/account/oauth", "", memberCookie), http.StatusUnauthorized)
	var listed struct{ Total int }
	response := perform(t, mux, "GET", "/auth/admin/users?search="+url.QueryEscape(email), "", adminCookie)
	assertStatus(t, response, http.StatusOK)
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil || listed.Total != 0 {
		t.Fatalf("deleted user still listed: %+v, %v", listed, err)
	}
	restored, err := service.CreatePasswordUser(t.Context(), email, "member-password-42")
	if err != nil || restored.ID == member.ID {
		t.Fatalf("deleted sign-in address was not released: %v", err)
	}
}
