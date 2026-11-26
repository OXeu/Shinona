package rest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/responsecache"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

func TestSessionListNegotiatesFullSnapshotsAndLargeDeltas(t *testing.T) {
	repo, router := restFixture(t)
	ctx := auth.WithUser(t.Context(), auth.User{ID: "alice"})
	now := time.Now().UTC()
	for i := range 20 {
		session := run.Session{ID: fmt.Sprintf("compression-%02d", i), OwnerID: "alice", Title: strings.Repeat("a", 160),
			CreatedAt: now, UpdatedAt: now.Add(time.Duration(i) * time.Second)}
		if err := repo.Conversations.CreateSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	poll := func(accept, tag string, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, Prefix+"/sessions?delta=true", nil)
		req.Header.Set("Accept-Encoding", accept)
		if tag != "" {
			req.Header.Set("If-None-Match", tag)
		}
		resp := serve(t, router, req, status)
		if !slices.Equal(resp.Header().Values("Vary"), []string{"Accept-Encoding"}) {
			t.Fatalf("unexpected Vary: %v", resp.Header())
		}
		return resp
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	checkEncoding := func(plain, compressed *httptest.ResponseRecorder) {
		t.Helper()
		if plain.Header().Get("Content-Encoding") != "" || compressed.Header().Get("Content-Encoding") != "zstd" ||
			compressed.Header().Get("ETag") != plain.Header().Get("ETag") {
			t.Fatalf("identity or encoded response selected wrong headers: %v, %v", plain.Header(), compressed.Header())
		}
		body, err := decoder.DecodeAll(compressed.Body.Bytes(), nil)
		if err != nil || !bytes.Equal(body, plain.Body.Bytes()) {
			t.Fatalf("encoded response changed the representation: %v", err)
		}
	}
	initial := poll("zstd;q=0, *;q=1", "", http.StatusOK)
	tag := initial.Header().Get("ETag")
	checkEncoding(initial, poll("zstd", "", http.StatusOK))
	unchanged := poll("zstd", tag, http.StatusNotModified)
	if unchanged.Body.Len() != 0 || unchanged.Header().Get("Content-Encoding") != "" {
		t.Fatalf("304 should have no wire body: %v", unchanged.Header())
	}

	for i := range 6 {
		title := strings.Repeat("b", 160)
		if _, err := repo.Conversations.UpdateSession(ctx, "alice", fmt.Sprintf("compression-%02d", i), run.SessionChanges{Title: &title}); err != nil {
			t.Fatal(err)
		}
	}
	delta := poll("identity", tag, http.StatusOK)
	patch := decode[sessionsPatch](t, delta).Delta.Sessions
	if delta.Body.Len() < httpserver.MinimumCompressSize || patch == nil || len(patch.Upsert) != 6 {
		t.Fatalf("expected a large delta with six changes, got %d bytes and %+v", delta.Body.Len(), patch)
	}
	checkEncoding(delta, poll("zstd", tag, http.StatusOK))
}

func TestSessionListHistoryKeepsRevisionIndexInStepWithEviction(t *testing.T) {
	alice, bob := sessionListScope{owner: "alice"}, sessionListScope{owner: "bob"}
	aliceOld := sessionListVersion{key: sessionListKey{alice, "former"}, revision: 1}
	aliceCurrent := sessionListVersion{key: sessionListKey{alice, "current"}, revision: 2}
	bobCurrent := sessionListVersion{key: sessionListKey{bob, "current"}, revision: 1}
	budget := aliceOld.retainedBytes() + aliceCurrent.retainedBytes()
	history := sessionListHistory{
		versions: responsecache.NewStore[sessionListKey, sessionListVersion](sessionListHistoryLimit, budget),
		latest:   make(map[sessionListScope]sessionListKey),
	}
	save := func(version sessionListVersion) { history.save(version, version.retainedBytes()) }
	save(aliceOld)
	save(aliceCurrent)
	if _, ok := history.get(aliceOld.key); !ok {
		t.Fatal("delta base disappeared before reaching capacity")
	}
	save(bobCurrent)
	if _, ok := history.latest[alice]; ok {
		t.Fatal("evicting Alice's latest version left its secondary index entry")
	}
	if _, ok := history.get(aliceOld.key); !ok {
		t.Fatal("evicting another version removed Alice's delta base")
	}
	save(aliceCurrent)
	save(aliceOld)
	if _, ok := history.currentLocked(alice, 2); !ok {
		t.Fatal("an older request completing later replaced the current revision")
	}
	save(sessionListVersion{key: aliceCurrent.key, revision: 3})
	save(aliceCurrent)
	if _, ok := history.currentLocked(alice, 3); !ok {
		t.Fatal("an older snapshot with the same ETag replaced the current revision")
	}
}

func TestSessionListSharesConcurrentBuildsByScopeAndRevision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var history sessionListHistory
		key := sessionListRevision{scope: sessionListScope{owner: "alice"}, revision: 3}
		version := sessionListVersion{key: sessionListKey{key.scope, "shared"}}
		release := make(chan struct{})
		calls := make(chan struct{}, 12)
		results := make(chan string, 12)
		for range cap(results) {
			go func() {
				got, err := history.load(t.Context(), key, func() (sessionListVersion, error) {
					calls <- struct{}{}
					<-release
					return version, nil
				})
				if err != nil {
					t.Error(err)
				}
				results <- got.key.etag
			}()
		}
		synctest.Wait()
		if len(calls) != 1 {
			t.Errorf("same session list revision ran %d builds", len(calls))
		}
		for _, independent := range []sessionListRevision{
			{scope: sessionListScope{owner: "bob"}, revision: 3},
			{scope: sessionListScope{owner: "alice", filtered: true}, revision: 3},
			{scope: key.scope, revision: 4},
		} {
			local := sessionListVersion{key: sessionListKey{independent.scope, "independent"}, revision: independent.revision}
			got, err := history.load(t.Context(), independent, func() (sessionListVersion, error) { return local, nil })
			if err != nil || got.key != local.key || got.revision != local.revision {
				t.Errorf("independent request %+v received %+v, %v", independent, got, err)
			}
		}
		close(release)
		for range cap(results) {
			if etag := <-results; etag != version.key.etag {
				t.Errorf("same revision received ETag %q", etag)
			}
		}
	})
}

func TestSessionListResumesWaitingRequestsAfterBuilderCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var history sessionListHistory
		key := sessionListRevision{scope: sessionListScope{owner: "alice"}, revision: 3}
		leader, cancelLeader := context.WithCancel(t.Context())
		defer cancelLeader()
		follower, cancelFollower := context.WithCancel(t.Context())
		defer cancelFollower()
		leaderErr, followerErr := make(chan error, 1), make(chan error, 1)
		started := make(chan struct{})
		go func() {
			_, err := history.load(leader, key, func() (sessionListVersion, error) {
				close(started)
				<-leader.Done()
				return sessionListVersion{}, leader.Err()
			})
			leaderErr <- err
		}()
		<-started
		go func() {
			_, err := history.load(follower, key, func() (sessionListVersion, error) {
				t.Error("a canceled follower ran the build")
				return sessionListVersion{}, nil
			})
			followerErr <- err
		}()
		recovered := make(chan string, 1)
		go func() {
			result, err := history.load(t.Context(), key, func() (sessionListVersion, error) {
				return sessionListVersion{key: sessionListKey{key.scope, "recovered"}}, nil
			})
			if err != nil {
				t.Error(err)
			}
			recovered <- result.key.etag
		}()
		synctest.Wait()
		cancelFollower()
		if err := <-followerErr; !errors.Is(err, context.Canceled) {
			t.Errorf("canceled follower returned %v", err)
		}
		cancelLeader()
		if err := <-leaderErr; !errors.Is(err, context.Canceled) {
			t.Errorf("canceled builder returned %v", err)
		}
		if etag := <-recovered; etag != "recovered" {
			t.Errorf("remaining request received ETag %q", etag)
		}
	})
}

func TestSessionListValidatorsWorkWithSnapshotCachingDisabled(t *testing.T) {
	config := DefaultConfig()
	config.SessionListCacheBytes = 0
	_, router := restFixtureWithConfig(t, config)
	session := decode[sessionSummary](t, perform(t, router, "alice", http.MethodPost, "/sessions", map[string]string{}, http.StatusCreated))
	ctx := auth.WithUser(t.Context(), auth.User{ID: "alice"})
	poll := func(tag string, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, Prefix+"/sessions?delta=true", nil)
		if tag != "" {
			req.Header.Set("If-None-Match", tag)
		}
		return serve(t, router, req, status)
	}
	initial := poll("", http.StatusOK)
	tag := initial.Header().Get("ETag")
	if tag == "" || poll(tag, http.StatusNotModified).Body.Len() != 0 {
		t.Fatal("a disabled cache changed the conditional response")
	}
	perform(t, router, "alice", http.MethodPatch, "/sessions/"+session.ID, map[string]string{"title": "Renamed"}, http.StatusOK)
	changed := poll(tag, http.StatusOK)
	list := decode[sessionsResponse](t, changed)
	if len(list.Sessions) != 1 || list.Sessions[0].Title != "Renamed" || changed.Header().Get("ETag") == tag {
		t.Fatalf("a missing delta base did not return a fresh full snapshot: %s", changed.Body)
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

	perform(t, router, owner, "PATCH", session, map[string]any{"title": "Renamed", "pinned": true}, 200)
	poll(owner, path, pinned.Header().Get("ETag"), http.StatusNotModified)

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
