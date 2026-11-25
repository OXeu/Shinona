// Package timing collects elapsed time for named phases of an operation.
package timing

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

type recorderKey struct{}

type Metric struct {
	Name     string
	Duration time.Duration
	Calls    int
}

type Recorder struct {
	mu      sync.Mutex
	metrics map[string]Metric
}

// WithRecorder starts an independent timing scope.
func WithRecorder(ctx context.Context) (context.Context, *Recorder) {
	recorder := &Recorder{metrics: make(map[string]Metric)}
	return context.WithValue(ctx, recorderKey{}, recorder), recorder
}

// Start measures a phase when the returned function is called. Names must be
// fixed by server code, for example a schema field, rather than request values.
func Start(ctx context.Context, name string) func() {
	recorder, _ := ctx.Value(recorderKey{}).(*Recorder)
	if recorder == nil {
		return func() {}
	}
	started := time.Now()
	return func() { recorder.add(name, time.Since(started)) }
}

// Since records a phase whose start time was captured by another component.
func Since(ctx context.Context, name string, started time.Time) {
	if recorder, ok := ctx.Value(recorderKey{}).(*Recorder); ok {
		recorder.add(name, time.Since(started))
	}
}

func (r *Recorder) add(name string, duration time.Duration) {
	r.merge(Metric{Name: name, Duration: duration, Calls: 1})
}

func (r *Recorder) merge(recorded Metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	metric := r.metrics[recorded.Name]
	metric.Name = recorded.Name
	metric.Duration += recorded.Duration
	metric.Calls += recorded.Calls
	r.metrics[recorded.Name] = metric
}

// Publish adds the collected phases to an enclosing operation.
func (r *Recorder) Publish(ctx context.Context) {
	if target, ok := ctx.Value(recorderKey{}).(*Recorder); ok && target != r {
		for _, metric := range r.Metrics() {
			target.merge(metric)
		}
	}
}

func (r *Recorder) Metrics() []Metric {
	r.mu.Lock()
	metrics := make([]Metric, 0, len(r.metrics))
	for _, metric := range r.metrics {
		metrics = append(metrics, metric)
	}
	r.mu.Unlock()
	sort.Slice(metrics, func(i, j int) bool { return metrics[i].Name < metrics[j].Name })
	return metrics
}

func (r *Recorder) LogValue() slog.Value {
	var attrs []slog.Attr
	for _, metric := range r.Metrics() {
		attrs = append(attrs, slog.Group(metric.Name, "ms", float64(metric.Duration)/float64(time.Millisecond), "calls", metric.Calls))
	}
	return slog.GroupValue(attrs...)
}
