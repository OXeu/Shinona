// Package httpserver provides shared HTTP server transport behavior.
package httpserver

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/OXeu/shinona/internal/timing"
)

const timingHeader = "Server-Timing"

// ServerTiming records the time from handler entry until the response starts.
func ServerTiming(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, phases := timing.WithRecorder(r.Context())
		response := &timedWriter{ResponseWriter: w, started: time.Now(), phases: phases}
		next.ServeHTTP(response, r.WithContext(ctx))
		response.commitTiming()
	})
}

type timedWriter struct {
	http.ResponseWriter
	started   time.Time
	phases    *timing.Recorder
	committed bool
}

func (w *timedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func metricValue(name string, duration time.Duration) string {
	milliseconds := float64(duration) / float64(time.Millisecond)
	return name + ";dur=" + strconv.FormatFloat(milliseconds, 'f', 3, 64)
}

func phaseValues(phases *timing.Recorder) []string {
	metrics := phases.Metrics()
	values := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		values = append(values, metricValue(metric.Name, metric.Duration))
	}
	return values
}

func (w *timedWriter) metrics() []string {
	return append([]string{metricValue("app", time.Since(w.started))}, phaseValues(w.phases)...)
}

func (w *timedWriter) commitTiming() {
	if !w.committed {
		for _, metric := range w.metrics() {
			w.Header().Add(timingHeader, metric)
		}
		w.committed = true
	}
}

func (w *timedWriter) WriteHeader(status int) {
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.commitTiming()
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *timedWriter) Write(body []byte) (int, error) {
	w.commitTiming()
	return w.ResponseWriter.Write(body)
}

func (w *timedWriter) FlushError() error {
	w.commitTiming()
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *timedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.committed = true
	}
	return conn, buffer, err
}

// WebSocketTimingHeaders supplies the timing for a handshake written directly to the connection.
func WebSocketTimingHeaders(w http.ResponseWriter) http.Header {
	if timed := findTimedWriter(w); timed != nil {
		return http.Header{timingHeader: timed.metrics()}
	}
	return nil
}

func findTimedWriter(w http.ResponseWriter) *timedWriter {
	for w != nil {
		if timed, ok := w.(*timedWriter); ok {
			return timed
		}
		wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = wrapper.Unwrap()
	}
	return nil
}

// StreamTiming collects phases that finish after the initial HTTP headers.
func StreamTiming(w http.ResponseWriter, ctx context.Context) (context.Context, func()) {
	timed := findTimedWriter(w)
	if timed == nil {
		return ctx, func() {}
	}
	ctx, phases := timing.WithRecorder(ctx)
	w.Header().Add("Trailer", timingHeader)
	return ctx, func() {
		if timed.committed {
			w.Header()[timingHeader] = phaseValues(phases)
		}
	}
}

// MaxBytesReader preserves net/http's handling of oversized requests through the timing writer.
func MaxBytesReader(w http.ResponseWriter, body io.ReadCloser, limit int64) io.ReadCloser {
	if timed := findTimedWriter(w); timed != nil {
		w = timed.Unwrap()
	}
	return http.MaxBytesReader(w, body, limit)
}

// MaxBytesHandler limits the request body before passing it to the handler.
func MaxBytesHandler(next http.Handler, limit int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := *r
		request.Body = MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, &request)
	})
}
