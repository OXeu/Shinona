package httpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OXeu/shinona/internal/timing"
)

func TestServerTimingHTTP(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add(timingHeader, "db;dur=0.1")
		timing.Start(r.Context(), "business_lookup")()
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /empty", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ok", http.StatusSeeOther)
	})
	handler := ServerTiming(mux)
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/ok", http.StatusOK},
		{http.MethodGet, "/empty", http.StatusOK},
		{http.MethodGet, "/redirect", http.StatusSeeOther},
		{http.MethodGet, "/missing", http.StatusNotFound},
		{http.MethodPost, "/ok", http.StatusMethodNotAllowed},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			writer := httptest.NewRecorder()
			handler.ServeHTTP(writer, httptest.NewRequest(test.method, test.path, nil))
			response := writer.Result()
			defer response.Body.Close()
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
			assertAppTiming(t, response.Header)
			if test.path == "/ok" && test.status == http.StatusOK && response.Header.Get(timingHeader) != "db;dur=0.1" {
				t.Fatal("existing timing metric was lost")
			}
			if test.path == "/ok" && test.status == http.StatusOK {
				assertTiming(t, response.Header, "business_lookup")
			}
		})
	}
}

func TestServerTimingFlush(t *testing.T) {
	finish := make(chan struct{})
	server := httptest.NewServer(ServerTiming(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		<-finish
	})))
	defer server.Close()
	defer close(finish)
	client := server.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	assertAppTiming(t, response.Header)
}

func TestServerTimingTrailers(t *testing.T) {
	server := httptest.NewServer(ServerTiming(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timing.Start(r.Context(), "transcript_load")()
		ctx, finish := StreamTiming(w, r.Context())
		defer finish()
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		timing.Start(ctx, "protobuf_encode")()
		_, _ = io.WriteString(w, "frame")
	})))
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	assertTiming(t, response.Header, "transcript_load")
	data, err := io.ReadAll(response.Body)
	if err != nil || string(data) != "frame" {
		t.Fatalf("body = %q, err = %v", data, err)
	}
	assertTiming(t, response.Trailer, "protobuf_encode")
	if len(response.Trailer.Values(timingHeader)) != 1 {
		t.Fatalf("trailer repeated a header phase: %v", response.Trailer.Values(timingHeader))
	}
}

func TestServerTimingWebSocket(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(ServerTiming(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, WebSocketTimingHeaders(w))
		if err == nil {
			_ = conn.Close()
		}
	})))
	defer server.Close()
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, response, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if response != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	assertAppTiming(t, response.Header)
}

func TestServerTimingRetainsOversizedRequestHandling(t *testing.T) {
	server := httptest.NewServer(ServerTiming(MaxBytesHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		}
	}), 4)))
	defer server.Close()
	response, err := server.Client().Post(server.URL, "text/plain", strings.NewReader("too large"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge || !response.Close {
		t.Fatalf("status = %d, connection close = %t", response.StatusCode, response.Close)
	}
	assertAppTiming(t, response.Header)
}

func assertAppTiming(t *testing.T, header http.Header) {
	t.Helper()
	assertTiming(t, header, "app")
}

func assertTiming(t *testing.T, header http.Header, name string) {
	t.Helper()
	var found int
	for _, value := range header.Values(timingHeader) {
		duration, ok := strings.CutPrefix(value, name+";dur=")
		if !ok {
			continue
		}
		if milliseconds, err := strconv.ParseFloat(duration, 64); err != nil || milliseconds < 0 {
			t.Fatalf("invalid %s duration %q", name, duration)
		}
		found++
	}
	if found != 1 {
		t.Fatalf("want one %s duration, got %v", name, header.Values(timingHeader))
	}
}
