package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func TestPendingMessageActions(t *testing.T) {
	agent := benchmarkSteeringAgent{input: make(chan run.Input, 1), release: make(chan struct{})}
	a := newCoreBenchmarkAPI(t, agent)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	session := a.session(t, "")
	submit := func(prompt string, images ...run.Image) run.Run {
		t.Helper()
		item, err := a.service.Submit(ctx, a.owner, session, prompt, run.Payload{Images: images}, true)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	path := func(id string) string { return benchmarkSessionPath(session) + "/turns/" + id }
	transcript := func() []run.Run {
		t.Helper()
		entry, err := a.service.Transcript(ctx, a.owner, session)
		if err != nil || entry == nil {
			t.Fatalf("read transcript: %v", err)
		}
		return entry.Turns
	}
	request := func(method, path string, body any, status int) []byte {
		t.Helper()
		var encoded []byte
		if body != nil {
			encoded = a.requestBody(t, body)
		}
		data, _, err := a.request(ctx, a.Client(), method, path, encoded, status)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first := submit("active")
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.NewWorker(a.repo, a.resolve, a.logger, a.controls).Serve(ctx)
	}()
	defer func() { cancel(); <-done }()
	var input run.Input
	select {
	case input = <-agent.input:
	case <-ctx.Done():
		t.Fatal("worker did not start the active turn")
	}

	activeSteering := path(first.ID) + "/steering"
	for _, prompt := range []string{"send as queued", "return as draft"} {
		request(http.MethodPost, activeSteering, map[string]string{"prompt": prompt}, http.StatusNoContent)
	}
	messages := transcript()[0].Steering
	if len(messages) != 2 || messages[0].ID == "" || messages[1].ID == "" || messages[0].ID == messages[1].ID {
		t.Fatalf("expected distinct steering IDs: %+v", messages)
	}

	promote := submit("send as steering")
	image := run.Image{Name: "image.png", MediaType: "image/png", Data: "iVBORw0KGgo="}
	withImage := submit("return with attachment", image)
	target := map[string]string{"runId": first.ID}
	request(http.MethodPost, path(withImage.ID)+"/steer", target, http.StatusBadRequest)
	request(http.MethodPost, path(promote.ID)+"/steer", map[string]string{"runId": "stale"}, http.StatusConflict)
	request(http.MethodPost, path(promote.ID)+"/steer", target, http.StatusNoContent)
	request(http.MethodDelete, path(promote.ID)+"/queue", nil, http.StatusConflict)

	request(http.MethodPost, activeSteering+"/"+messages[0].ID+"/queue", map[string]any{"options": map[string]string{"model": "next"}}, http.StatusNoContent)
	turns := transcript()
	if len(turns) != 3 || turns[1].ID != withImage.ID || turns[2].Status != run.Queued || turns[2].Prompt != messages[0].Text ||
		turns[2].Payload.Options.Model != "next" {
		t.Fatalf("unexpected queue: %+v", turns)
	}
	data := request(http.MethodDelete, activeSteering+"/"+messages[1].ID, nil, http.StatusOK)
	var draft run.PendingDraft
	if err := json.Unmarshal(data, &draft); err != nil || draft.Prompt != messages[1].Text {
		t.Fatalf("unexpected steering draft: %+v, %v", draft, err)
	}
	data = request(http.MethodDelete, path(withImage.ID)+"/queue", nil, http.StatusOK)
	if err := json.Unmarshal(data, &draft); err != nil || draft.Prompt != withImage.Prompt || !slices.Equal(draft.Images, []run.Image{image}) {
		t.Fatalf("unexpected queued draft: %+v, %v", draft, err)
	}
	if delivered := input.Steering.Drain(); !slices.Equal(delivered, []string{promote.Prompt}) {
		t.Fatalf("unexpected delivered steering: %v", delivered)
	}
	request(http.MethodDelete, activeSteering+"/"+promote.ID, nil, http.StatusConflict)
	request(http.MethodDelete, path(turns[2].ID)+"/queue", nil, http.StatusOK)
	entry, err := a.service.Transcript(ctx, a.owner, session)
	if err != nil || entry == nil || len(entry.Turns) != 1 || entry.Session.TurnCount != first.Sequence {
		t.Fatalf("deleting queue entries left a stale head: %+v, %v", entry, err)
	}
}
