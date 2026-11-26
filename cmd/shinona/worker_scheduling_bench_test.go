package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/adapter/password"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

type benchmarkAgentInterval struct {
	prompt     string
	start, end time.Time
	history    int
}

type benchmarkTimedAgent struct {
	intervals chan benchmarkAgentInterval
}

func (a benchmarkTimedAgent) Run(ctx context.Context, input run.Input, _ sandbox.Workspace) (run.Result, error) {
	start := time.Now()
	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return run.Result{}, ctx.Err()
	case <-timer.C:
		a.intervals <- benchmarkAgentInterval{prompt: input.Prompt, start: start, end: time.Now(), history: len(input.History)}
		return run.Result{}, nil
	}
}

type benchmarkSchedulingRepository struct {
	run.Repository
	completions chan run.Status
}

type schedulingTestRepository struct {
	run.Repository
	idle chan struct{}
}

func (r schedulingTestRepository) ClaimNext(ctx context.Context, now time.Time) (*run.Run, error) {
	item, err := r.Repository.ClaimNext(ctx, now)
	if item == nil && err == nil {
		select {
		case r.idle <- struct{}{}:
		default:
		}
	}
	return item, err
}

func TestWorkerWakesAndRunsIndependentWorkspaces(t *testing.T) {
	agent := benchmarkSteeringAgent{input: make(chan run.Input, 4), release: make(chan struct{})}
	a := newCoreBenchmarkAPI(t, agent)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	repo := schedulingTestRepository{a.repo, make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.NewWorker(repo, a.resolve, a.logger, a.controls, run.DefaultWorkerConcurrency).Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-repo.idle:
	case <-ctx.Done():
		t.Fatal("worker did not reach the idle queue")
	}
	project, err := a.service.CreateProject(ctx, a.owner, "Independent workspace")
	if err != nil {
		t.Fatal(err)
	}
	account, err := auth.New(a.repo, password.Argon2id{}, uuid.NewString).
		CreatePasswordUser(ctx, "parallel@example.invalid", "local benchmark password")
	if err != nil {
		t.Fatal(err)
	}
	other, err := a.service.CreateSession(ctx, account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitStart := func() string {
		t.Helper()
		select {
		case input := <-agent.input:
			return input.Prompt
		case <-ctx.Done():
			t.Fatal("worker did not start eligible work before the periodic fallback")
			return ""
		}
	}
	submit := func(owner, session, text string) {
		t.Helper()
		if _, err := a.service.Submit(ctx, owner, session, text, run.Payload{}, false); err != nil {
			t.Fatal(err)
		}
	}
	submit(a.owner, a.session(t, ""), "held")
	if start := waitStart(); start != "held" {
		t.Fatalf("unexpected first turn: %s", start)
	}
	submit(a.owner, a.session(t, ""), "same project")
	submit(a.owner, a.session(t, project.ID), "independent project")
	submit(account.ID, other.ID, "independent account")
	left, right := waitStart(), waitStart()
	first, second := left, right
	if first > second {
		first, second = second, first
	}
	if first != "independent account" || second != "independent project" {
		t.Fatalf("the busy workspace competed with eligible work: %q, %q", left, right)
	}
	close(agent.release)
	if start := waitStart(); start != "same project" {
		t.Fatalf("unexpected turn after release: %s", start)
	}
}

func (r benchmarkSchedulingRepository) Complete(ctx context.Context, id string, completion run.Completion) error {
	err := r.Repository.Complete(ctx, id, completion)
	if err != nil {
		r.completions <- run.Failed
	} else {
		r.completions <- completion.Status
	}
	return err
}

func benchmarkWorkerScheduling(b *testing.B) {
	for _, fixture := range []struct {
		name        string
		sameSession bool
	}{{"SameSession", true}, {"SeparateAccounts", false}} {
		b.Run(fixture.name, func(b *testing.B) {
			benchmarkWorkerPair(b, fixture.sameSession)
		})
	}
}

func benchmarkWorkerPair(b *testing.B, sameSession bool) {
	agent := benchmarkTimedAgent{intervals: make(chan benchmarkAgentInterval, 2)}
	a := newCoreBenchmarkAPI(b, agent)
	secondOwner := a.owner
	if !sameSession {
		accounts := auth.New(a.repo, password.Argon2id{}, uuid.NewString)
		account, err := accounts.CreatePasswordUser(b.Context(), "second-benchmark@example.invalid", "local benchmark password")
		if err != nil {
			b.Fatal(err)
		}
		secondOwner = account.ID
	}
	repo := benchmarkSchedulingRepository{Repository: a.repo, completions: make(chan run.Status, 2)}
	ctx, cancel := context.WithCancel(b.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.NewWorker(repo, a.resolve, a.logger, a.controls, run.DefaultWorkerConcurrency).Serve(ctx)
	}()
	b.Cleanup(func() {
		cancel()
		<-done
	})
	var overlap int
	var startDelay time.Duration
	for b.Loop() {
		b.StopTimer()
		first := a.session(b, "")
		second := first
		if !sameSession {
			session, err := a.service.CreateSession(ctx, secondOwner, "")
			if err != nil {
				b.Fatal(err)
			}
			second = session.ID
		}
		b.StartTimer()
		submittedAt := time.Now()
		if _, err := a.service.Submit(ctx, a.owner, first, "first", run.Payload{}, false); err != nil {
			b.Fatal(err)
		}
		if _, err := a.service.Submit(ctx, secondOwner, second, "second", run.Payload{}, sameSession); err != nil {
			b.Fatal(err)
		}
		timeout := time.NewTimer(5 * time.Second)
		for range 2 {
			select {
			case status := <-repo.completions:
				if status != run.Succeeded {
					b.Fatalf("fixture run ended with status %s", status)
				}
			case <-timeout.C:
				b.Fatal("worker did not complete both fixture runs")
			}
		}
		timeout.Stop()
		left, right := <-agent.intervals, <-agent.intervals
		concurrent := left.start.Before(right.end) && right.start.Before(left.end)
		if sameSession && (concurrent || left.prompt != "first" || right.history != 1) {
			b.Fatalf("same-session turns did not run in submission order: %+v, %+v", left, right)
		}
		if concurrent {
			overlap++
		}
		startDelay += min(left.start.Sub(submittedAt), right.start.Sub(submittedAt))
	}
	b.ReportMetric(100*float64(overlap)/float64(b.N), "overlap-%")
	b.ReportMetric(float64(startDelay.Microseconds())/float64(b.N), "start-us/op")
}
