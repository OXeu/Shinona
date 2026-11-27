package terminal

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestTerminalRevalidationDistinguishesRevocationFromLookupFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{name: "revoked", code: signIn},
		{name: "temporary lookup failure", err: errors.New("private database error"), code: websocket.CloseInternalServerErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				socket, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade terminal: %v", err)
					return
				}
				defer func() { _ = socket.Close() }()
				check, stop := context.WithCancel(r.Context())
				keepAlive(check, socket, stop, slog.New(slog.DiscardHandler), time.Millisecond, func(context.Context) (bool, error) {
					return false, tc.err
				})
			}))
			t.Cleanup(server.Close)
			socket, response, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if response != nil {
				_ = response.Body.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = socket.Close() }()
			deadline, _ := ctx.Deadline()
			if err := socket.SetReadDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			_, _, err = socket.ReadMessage()
			if !websocket.IsCloseError(err, tc.code) || strings.Contains(err.Error(), "private database error") {
				t.Fatalf("terminal closed with %v, want code %d", err, tc.code)
			}
		})
	}
}
