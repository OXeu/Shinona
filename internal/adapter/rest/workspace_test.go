package rest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

func TestWorkspaceCommitRejectsAStaleSelection(t *testing.T) {
	ctx := t.Context()
	repo, _ := restFixture(t)
	service := fixtureService(repo.RunStores())
	const owner = "alice"
	project, err := service.CreateProject(ctx, owner, "removing")
	if err != nil {
		t.Fatal(err)
	}
	moving, err := service.CreateSession(ctx, owner, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := service.CreateSession(ctx, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Projects.SetWorkspaceRoot(ctx, owner, moving.ID, project.ID, "default-repo"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Projects.SetWorkspaceRoot(ctx, owner, fallback.ID, "", "default-repo"); err != nil {
		t.Fatal(err)
	}
	stores := repo.RunStores()
	sessions := &movingWorkspaceSession{SessionRepository: stores.Sessions}
	stores.Sessions = sessions
	box := &commitWorkspaceFixture{}
	provider := &commitProjectFixture{box: box, projectID: project.ID}
	resolve := func(context.Context, string, string) (run.Target, error) { return run.Target{Provider: provider}, nil }
	service = run.NewService(stores, resolve, uuid.NewString, &run.Controls{}, nil, run.DefaultWorkspaceTimeouts())
	router := gin.New()
	Register(router.Group(Prefix), service, fixtureApplication{}, slog.New(slog.DiscardHandler), DefaultConfig())
	path := "/sessions/" + moving.ID + "/workspace/"
	request := map[string]any{"message": "save work", "paths": []string{"patch.txt"}}
	rejectCommit := func() {
		t.Helper()
		before := box.commands
		response := perform(t, router, owner, http.MethodPost, path+"commits", request, http.StatusConflict)
		checkCode(t, response, "WORKSPACE_SELECTION_CHANGED")
		if box.commands != before {
			t.Fatal("ran Git before checking the workspace selection")
		}
	}
	readChanges := func() workspaceChangesResponse {
		t.Helper()
		return decode[workspaceChangesResponse](t, perform(t, router, owner, http.MethodGet, path+"changes", nil, http.StatusOK))
	}
	listed := readChanges()
	if listed.Selection != (workspaceSelection{ProjectID: project.ID, Root: "default-repo"}) {
		t.Fatalf("wrong initial workspace selection: %+v", listed)
	}
	request["selection"] = listed.Selection
	sessions.move = func(ctx context.Context) error { return repo.Projects.RemoveProject(ctx, owner, project.ID) }
	provider.projectID = ""
	rejectCommit()

	listed = readChanges()
	if listed.Selection != (workspaceSelection{Root: "default-repo"}) {
		t.Fatalf("changes did not include the leased workspace selection: %+v", listed)
	}
	request["selection"] = listed.Selection
	if err := repo.Projects.SetWorkspaceRoot(ctx, owner, fallback.ID, "", "other-repo"); err != nil {
		t.Fatal(err)
	}
	rejectCommit()
	delete(request, "selection")
	rejectCommit()
	if err := repo.Projects.SetWorkspaceRoot(ctx, owner, fallback.ID, "", "default-repo"); err != nil {
		t.Fatal(err)
	}
	request["selection"] = listed.Selection
	response := perform(t, router, owner, http.MethodPost, path+"commits", request, http.StatusCreated)
	if got := decode[string](t, response); got != "abcd123" || !box.committed {
		t.Fatalf("unexpected commit result: %q", got)
	}
}

func TestWorkspaceFilesKeepSessionOwnershipChecks(t *testing.T) {
	repo, router := restFixture(t)
	service := fixtureService(repo.RunStores())
	session, err := service.CreateSession(t.Context(), "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"files?path=.", "preview?path=file.txt"} {
		response := perform(t, router, "bob", http.MethodGet, "/sessions/"+session.ID+"/workspace/"+route, nil, http.StatusNotFound)
		checkCode(t, response, "SESSION_NOT_FOUND")
	}
}

type movingWorkspaceSession struct {
	run.SessionRepository
	move func(context.Context) error
}

func (r *movingWorkspaceSession) SessionProject(ctx context.Context, owner, id string) (string, error) {
	if move := r.move; move != nil {
		r.move = nil
		if err := move(ctx); err != nil {
			return "", err
		}
	}
	return r.SessionRepository.SessionProject(ctx, owner, id)
}

type commitProjectFixture struct {
	sandbox.ProjectProvider
	box       *commitWorkspaceFixture
	projectID string
}

func (p *commitProjectFixture) OpenProject(_ context.Context, spec sandbox.Spec) (sandbox.Workspace, error) {
	if spec.ProjectID != p.projectID {
		return nil, fmt.Errorf("workspace acquired for project %q instead of %q", spec.ProjectID, p.projectID)
	}
	return p.box, nil
}

type commitWorkspaceFixture struct {
	sandbox.Workspace
	committed bool
	commands  int
}

func (b *commitWorkspaceFixture) Close(context.Context) error { return nil }

func (b *commitWorkspaceFixture) Exec(_ context.Context, cmd sandbox.Command) (sandbox.Result, error) {
	b.commands++
	if cmd.Program != "sh" || len(cmd.Args) < 5 || cmd.Args[3] != "default-repo" {
		return sandbox.Result{}, fmt.Errorf("Git called in the wrong workspace directory: %q", cmd.Args)
	}
	switch {
	case slices.Contains(cmd.Args, "--show-prefix"):
		return sandbox.Result{Stdout: "true\n\n"}, nil
	case slices.Contains(cmd.Args, "symbolic-ref"):
		return sandbox.Result{Stdout: "main\n"}, nil
	case slices.Contains(cmd.Args, "status"):
		return sandbox.Result{Stdout: " M patch.txt\x00"}, nil
	case slices.Contains(cmd.Args, "--only"):
		b.committed = true
		return sandbox.Result{}, nil
	case slices.Contains(cmd.Args, "--short") && b.committed:
		return sandbox.Result{Stdout: "abcd123\n"}, nil
	default:
		return sandbox.Result{}, fmt.Errorf("unexpected Git arguments: %q", cmd.Args)
	}
}
