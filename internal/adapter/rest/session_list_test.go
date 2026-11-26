package rest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/responsecache"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

func TestSessionListHistoryKeepsRevisionIndexInStepWithEviction(t *testing.T) {
	history := sessionListHistory{
		versions: responsecache.NewStore[sessionListKey, sessionListVersion](2, 2),
		latest:   make(map[sessionListScope]sessionListKey),
	}
	alice, bob := sessionListScope{owner: "alice"}, sessionListScope{owner: "bob"}
	aliceOld := sessionListVersion{key: sessionListKey{alice, "former"}, revision: 1}
	aliceCurrent := sessionListVersion{key: sessionListKey{alice, "current"}, revision: 2}
	bobCurrent := sessionListVersion{key: sessionListKey{bob, "current"}, revision: 1}
	history.save(aliceOld)
	history.save(aliceCurrent)
	if _, ok := history.get(aliceOld.key); !ok {
		t.Fatal("delta base disappeared before reaching capacity")
	}
	history.save(bobCurrent)
	if _, ok := history.latest[alice]; ok {
		t.Fatal("evicting Alice's latest version left its secondary index entry")
	}
	if _, ok := history.get(aliceOld.key); !ok {
		t.Fatal("evicting another version removed Alice's delta base")
	}
	history.save(aliceCurrent)
	history.save(aliceOld)
	if _, ok := history.current(alice, 2); !ok {
		t.Fatal("an older request completing later replaced the current revision")
	}
	history.save(sessionListVersion{key: aliceCurrent.key, revision: -1})
	if _, ok := history.current(alice, 2); !ok {
		t.Fatal("saving an unstable snapshot with the same ETag removed the current revision")
	}
}

func TestSessionListPolls(t *testing.T) {
	repo, router := restFixture(t)
	const owner = "alice"
	project := decode[projectResponse](t, perform(t, router, owner, "POST", "/projects", map[string]string{"name": "First project"}, 201))
	now := time.Now().UTC().Truncate(time.Second)
	for i := range 31 {
		session := run.Session{ID: fmt.Sprintf("history-%02d", i), OwnerID: owner, ProjectID: project.ID,
			Title: "Recorded conversation", CreatedAt: now, UpdatedAt: now.Add(time.Duration(i) * time.Second)}
		if err := repo.Conversations.CreateSession(t.Context(), session); err != nil {
			t.Fatal(err)
		}
	}
	const path = "/sessions?delta=true"
	poll := func(owner, path, tag string, status int) *httptest.ResponseRecorder {
		t.Helper()
		ctx := auth.WithUser(t.Context(), auth.User{ID: owner})
		ctx, recorder := timing.WithRecorder(ctx)
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, Prefix+path, nil)
		if tag != "" {
			request.Header.Set("If-None-Match", tag)
		}
		response := serve(t, router, request, status)
		if status == http.StatusNotModified {
			metrics := recorder.Metrics()
			if len(metrics) != 1 || metrics[0].Name != "sqlite" || metrics[0].Calls != 1 {
				t.Fatalf("unchanged list should check only its revision: %+v", metrics)
			}
		}
		return response
	}
	initial := poll(owner, path, "", http.StatusOK)
	full := decode[sessionsResponse](t, initial)
	tag := initial.Header().Get("ETag")
	if len(full.Sessions) != 30 || len(full.Projects) != 1 || !strings.HasPrefix(tag, `W/"`) {
		t.Fatalf("missing list or validator: %d sessions, %d projects, %q", len(full.Sessions), len(full.Projects), tag)
	}
	check := poll(owner, path, `"unrelated,*,tag", `+strings.TrimPrefix(tag, "W/"), http.StatusNotModified)
	if check.Body.Len() != 0 || check.Header().Get("ETag") != tag || check.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("conditional response = %v %q", check.Header(), check.Body)
	}
	staleTab := httptest.NewRequestWithContext(auth.WithUser(t.Context(), auth.User{ID: owner}), http.MethodGet, Prefix+path, nil)
	staleTab.Header.Set("X-Shinona-Account", "bob")
	staleTab.Header.Set("If-None-Match", tag)
	rejected := serve(t, router, staleTab, http.StatusUnauthorized)
	if rejected.Header().Get("X-Shinona-Error-Code") != "AUTH_REQUIRED" ||
		rejected.Header().Get("ETag") != "" || strings.Contains(rejected.Body.String(), "Recorded conversation") {
		t.Fatalf("account mismatch leaked session data: %v %q", rejected.Header(), rejected.Body)
	}
	staleTab.Header.Set("X-Shinona-Account", owner)
	serve(t, router, staleTab, http.StatusNotModified)

	const session = "/sessions/history-29"
	perform(t, router, owner, "PATCH", session, map[string]string{"title": "Renamed"}, 200)
	renamed := poll(owner, path, tag, http.StatusOK)
	patch := decode[sessionsPatch](t, renamed).Delta
	if patch.Projects != nil || patch.Sessions == nil || patch.Sessions.Order != nil ||
		len(patch.Sessions.Upsert) != 1 || patch.Sessions.Upsert[0].Title != "Renamed" ||
		!patch.Sessions.Upsert[0].UpdatedAt.Equal(full.Sessions[1].UpdatedAt) || renamed.Header().Get("ETag") == tag ||
		renamed.Body.Len() >= initial.Body.Len()/4 {
		t.Fatalf("rename returned an unexpected patch: %s", renamed.Body)
	}
	t.Logf("full list %d bytes, unchanged 0 bytes, single rename %d bytes", initial.Body.Len(), renamed.Body.Len())

	perform(t, router, owner, "PATCH", session, map[string]bool{"pinned": true}, 200)
	pinned := poll(owner, path, renamed.Header().Get("ETag"), http.StatusOK)
	patch = decode[sessionsPatch](t, pinned).Delta
	if patch.Sessions == nil || patch.Sessions.Order == nil || (*patch.Sessions.Order)[0] != "history-29" ||
		len(patch.Sessions.Upsert) != 1 || !patch.Sessions.Upsert[0].Pinned {
		t.Fatalf("pin returned an unexpected patch: %s", pinned.Body)
	}

	perform(t, router, owner, "DELETE", session, nil, 204)
	archived := poll(owner, path, pinned.Header().Get("ETag"), http.StatusOK)
	patch = decode[sessionsPatch](t, archived).Delta
	if patch.Sessions == nil || patch.Sessions.Order == nil || len(*patch.Sessions.Order) != 30 ||
		(*patch.Sessions.Order)[0] != "history-30" || (*patch.Sessions.Order)[29] != "history-00" ||
		len(patch.Sessions.Upsert) != 1 || patch.Sessions.Upsert[0].ID != "history-00" {
		t.Fatalf("archive failed to refill the list: %s", archived.Body)
	}

	perform(t, router, owner, "PATCH", "/projects/"+project.ID, map[string]string{"name": "New project name"}, 200)
	projectChanged := poll(owner, path, archived.Header().Get("ETag"), http.StatusOK)
	patch = decode[sessionsPatch](t, projectChanged).Delta
	if patch.Sessions != nil || patch.Projects == nil ||
		len(patch.Projects.Upsert) != 1 || patch.Projects.Upsert[0].Name != "New project name" {
		t.Fatalf("project rename returned an unexpected patch: %s", projectChanged.Body)
	}

	const activeSession = "/sessions/history-30"
	turn := decode[turnResponse](t, perform(t, router, owner, "POST", activeSession+"/turns", map[string]string{"prompt": "Start a turn"}, 202))
	updatedSession := func(tag string) (string, sessionSummary) {
		t.Helper()
		response := poll(owner, path, tag, http.StatusOK)
		patch := decode[sessionsPatch](t, response).Delta.Sessions
		if patch == nil || len(patch.Upsert) != 1 || patch.Upsert[0].ID != "history-30" {
			t.Fatalf("expected one updated session: %s", response.Body)
		}
		return response.Header().Get("ETag"), patch.Upsert[0]
	}
	activeTag, active := updatedSession(projectChanged.Header().Get("ETag"))
	if !active.Active || active.Unread {
		t.Fatalf("unexpected active session flags: %+v", active)
	}
	if _, err := repo.Conversations.ClaimNext(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Conversations.Complete(t.Context(), turn.ID, run.Completion{Status: run.Succeeded, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	unreadTag, unread := updatedSession(activeTag)
	if unread.Active || !unread.Unread {
		t.Fatalf("unexpected completed session flags: %+v", unread)
	}
	perform(t, router, owner, "PUT", activeSession+"/read-marker", map[string]any{"through": turn.Sequence, "throughId": turn.ID}, 204)
	_, read := updatedSession(unreadTag)
	if read.Unread || !read.UpdatedAt.Equal(unread.UpdatedAt) {
		t.Fatalf("unexpected read session: %+v", read)
	}

	for _, request := range []struct{ owner, path, tag string }{
		{owner, "/sessions", tag},
		{owner, path, `"unknown"`},
		{owner, path + "&projectId=" + url.QueryEscape(project.ID), tag},
		{"bob", path, tag},
	} {
		response := poll(request.owner, request.path, request.tag, http.StatusOK)
		list := decode[sessionsResponse](t, response)
		if list.Sessions == nil || list.Projects == nil ||
			(request.owner == "bob" && (len(list.Sessions) != 0 || len(list.Projects) != 0)) {
			t.Fatalf("expected scoped full snapshot: %s", response.Body)
		}
	}
}
