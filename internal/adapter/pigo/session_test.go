package pigo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
)

func TestPiDeliversSteeringBatchesAcrossModelRequests(t *testing.T) {
	lifetime, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	requests := make(chan []string)
	replies := make(chan string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct {
				Role    string
				Content json.RawMessage
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var userMessages []string
		for _, message := range input.Messages {
			if message.Role != "user" {
				continue
			}
			var text string
			if err := json.Unmarshal(message.Content, &text); err == nil {
				userMessages = append(userMessages, text)
				continue
			}
			var blocks []struct{ Text string }
			if err := json.Unmarshal(message.Content, &blocks); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, block := range blocks {
				userMessages = append(userMessages, block.Text)
			}
		}
		select {
		case requests <- userMessages:
		case <-r.Context().Done():
			return
		}
		select {
		case reply := <-replies:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", reply)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)

	endpoint, err := NewEndpoint(server.URL, "fixture", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := sqlite.Open(lifetime, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	repository := steeringRepository{ConversationStore: repo.Conversations, complete: make(chan run.Completion, 1)}
	controls := &run.Controls{}
	resolve := func(context.Context, string, string) (run.Target, error) {
		return run.Target{Name: "demo", Provider: demo.Provider{},
			ResolveAgent: func(context.Context, string) (run.Agent, error) { return New(endpoint, "fixture", nil), nil }}, nil
	}
	service := run.NewService(repo.RunStores(), resolve, uuid.NewString, controls, nil)
	const owner, prompt = "owner", "Inspect the test."
	if err := repo.Accounts.CreatePasswordUser(lifetime, auth.User{ID: owner, Email: "owner@example.test"}, []byte("test-hash")); err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(lifetime, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	item, err := service.Submit(lifetime, owner, session.ID, prompt, run.Payload{}, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.NewWorker(repository, repo.Projects, resolve, slog.New(slog.DiscardHandler), controls, 1).Serve(lifetime)
	}()
	defer func() {
		cancel()
		<-done
	}()

	assertRequest := func(want ...string) {
		t.Helper()
		select {
		case got := <-requests:
			if !slices.Equal(got, want) {
				t.Fatalf("got user messages %v, want %v", got, want)
			}
		case <-lifetime.Done():
			t.Fatal("Pi did not send the next request")
		}
	}
	steer := func(messages ...string) {
		t.Helper()
		for _, text := range messages {
			if err := service.Steer(owner, session.ID, item.ID, text); err != nil {
				t.Fatal(err)
			}
		}
	}
	reply := func(message string) {
		t.Helper()
		select {
		case replies <- message:
		case <-lifetime.Done():
			t.Fatal("Pi did not read the response")
		}
	}

	assertRequest(prompt)
	steer("First tool instruction.", "Second tool instruction.")
	reply(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"test.txt\"}"}}]},"finish_reason":"tool_calls"}]}`)
	assertRequest(prompt, "First tool instruction.", "Second tool instruction.")
	steer("First follow-up.", "Second follow-up.")
	reply(`{"choices":[{"index":0,"delta":{"content":"Initial answer."},"finish_reason":"stop"}]}`)
	assertRequest(prompt, "First tool instruction.", "Second tool instruction.", "First follow-up.", "Second follow-up.")
	reply(`{"choices":[{"index":0,"delta":{"content":"Final answer."},"finish_reason":"stop"}]}`)
	select {
	case completed := <-repository.complete:
		if completed.Status != run.Succeeded || completed.Output != "Final answer." || len(completed.Steering) != 4 {
			t.Fatalf("unexpected completion: %+v", completed)
		}
		for _, message := range completed.Steering {
			if !message.Delivered {
				t.Fatalf("steering was not marked delivered: %+v", message)
			}
		}
	case <-lifetime.Done():
		t.Fatal("turn did not complete after the final response")
	}
}

type steeringRepository struct {
	*sqlite.ConversationStore
	complete chan run.Completion
}

func (r steeringRepository) Complete(ctx context.Context, id string, completion run.Completion) error {
	if err := r.ConversationStore.Complete(ctx, id, completion); err != nil {
		return err
	}
	r.complete <- completion
	return nil
}
