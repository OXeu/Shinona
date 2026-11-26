package httpauth_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/OXeu/shinona/internal/adapter/httpauth"
)

func TestPasswordRateLimitsUseTheClientIP(t *testing.T) {
	limits := httpauth.DefaultLimits()
	limits.RateLimitEntries = 3
	handler := httpauth.NewWithConfig(nil, func() httpauth.Config {
		return httpauth.Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}}
	}, limits, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	handler.Routes(mux, func(h http.Handler) http.Handler { return h })

	try := func(peer, forwarded string, want int) {
		t.Helper()
		// Malformed JSON consumes the rate limit before password verification.
		request := httptest.NewRequest(http.MethodPost, "/auth/password", strings.NewReader("{"))
		request.RemoteAddr = peer
		request.Header.Set("X-Forwarded-For", forwarded)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("password request from %s, forwarded %q: got %d, want %d", peer, forwarded, response.Code, want)
		}
	}
	for i := range 5 {
		try(fmt.Sprintf("192.0.2.1:%d", 1000+i), "", http.StatusBadRequest)
		try("10.0.0.1:4321", "192.0.2.2, 10.0.0.2", http.StatusBadRequest)
	}
	try("192.0.2.1:2345", "192.0.2.3", http.StatusTooManyRequests)
	try("10.0.0.1:4321", "192.0.2.3, 192.0.2.2, 10.0.0.2", http.StatusTooManyRequests)
	try("10.0.0.1:4321", "192.0.2.3, 10.0.0.2", http.StatusBadRequest)
	for i := range 5 {
		try(fmt.Sprintf("[2001:db8:1::%x]:4321", i+1), "", http.StatusBadRequest)
	}
	try("[2001:db8:1::10]:4321", "", http.StatusTooManyRequests)
	// The fourth bucket evicts the least recently used address.
	try("192.0.2.1:4321", "", http.StatusBadRequest)
}
