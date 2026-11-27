package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestEnvironmentEmptyVariableOverridesDotEnv(t *testing.T) {
	const key = "SHINONA_TEST_EMPTY_ENVIRONMENT_OVERRIDE"
	values := environment{key: "from .env"}
	t.Setenv(key, "")
	if value, ok := values.lookup(key); value != "" || !ok {
		t.Fatalf("explicit empty override = %q, %t", value, ok)
	}
}

func unsetTestEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestAPICacheBudgetsUseMiBAndAcceptZero(t *testing.T) {
	const responses = "SHINONA_TRANSCRIPT_RESPONSE_CACHE_MIB"
	const turns = "SHINONA_TRANSCRIPT_TURN_CACHE_MIB"
	const lists = "SHINONA_SESSION_LIST_CACHE_MIB"
	unsetTestEnv(t, responses)
	unsetTestEnv(t, turns)
	unsetTestEnv(t, lists)
	config := environment{responses: "0", turns: "7", lists: "3"}
	got, err := apiConfig(config)
	if err != nil || got.Transcript.ResponseCacheBytes != 0 || got.Transcript.TurnCacheBytes != 7<<20 || got.SessionListCacheBytes != 3<<20 {
		t.Fatalf("configured API cache budgets = %+v, %v", got, err)
	}
	t.Setenv(lists, "0")
	if got, err := apiConfig(config); err != nil || got.SessionListCacheBytes != 0 {
		t.Fatalf("zero session-list cache budget = %+v, %v", got, err)
	}
	t.Setenv(lists, "-1")
	if _, err := apiConfig(config); err == nil || !strings.Contains(err.Error(), lists) {
		t.Fatalf("negative session-list cache budget returned %v", err)
	}
	unsetTestEnv(t, lists)
	t.Setenv(turns, "513")
	if _, err := apiConfig(config); err == nil || !strings.Contains(err.Error(), turns) {
		t.Fatalf("oversized turn cache returned %v", err)
	}
	t.Setenv("SHINONA_SQLITE_TRANSCRIPT_CACHE_MIB", "-1")
	if _, err := openDatabase(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "SHINONA_SQLITE_TRANSCRIPT_CACHE_MIB") {
		t.Fatalf("negative SQLite transcript cache returned %v", err)
	}
}

func TestOpenDatabaseRejectsInvalidReadConnections(t *testing.T) {
	const readers = "SHINONA_SQLITE_READ_CONNECTIONS"
	t.Setenv("SHINONA_DATABASE", ":memory:")
	unsetTestEnv(t, readers)
	repo, err := openDatabase(t.Context(), environment{readers: "3"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	t.Setenv(readers, "0")
	_, err = openDatabase(t.Context(), environment{readers: "3"})
	if err == nil || !strings.Contains(err.Error(), readers) {
		t.Fatalf("opening with invalid read connections returned %v", err)
	}
}

func TestWorkspaceTimeoutsReadStartupOverrides(t *testing.T) {
	const operation = "SHINONA_WORKSPACE_OPERATION_TIMEOUT_SECONDS"
	const lifecycle = "SHINONA_PROJECT_SANDBOX_TIMEOUT_SECONDS"
	const cleanup = "SHINONA_WORKSPACE_CLEANUP_TIMEOUT_SECONDS"
	unsetTestEnv(t, operation)
	t.Setenv(lifecycle, "150")
	unsetTestEnv(t, cleanup)
	config := environment{operation: "30", cleanup: "8"}
	limits, err := workspaceTimeouts(config)
	if err != nil || limits.Operation != 30*time.Second || limits.Lifecycle != 150*time.Second || limits.Cleanup != 8*time.Second {
		t.Fatalf("configured workspace timeouts = %+v, %v", limits, err)
	}
	t.Setenv(cleanup, "0")
	if _, err := workspaceTimeouts(config); err == nil {
		t.Fatal("accepted a zero workspace cleanup timeout")
	}
}

func TestAuthLimitsReadStartupOverrides(t *testing.T) {
	const passwordKey = "SHINONA_AUTH_PASSWORD_CONCURRENCY"
	const rateKey = "SHINONA_AUTH_RATE_LIMIT_ENTRIES"
	unsetTestEnv(t, passwordKey)
	unsetTestEnv(t, rateKey)
	config := environment{passwordKey: "6", rateKey: "4096"}
	t.Setenv(passwordKey, "8")
	limits, err := authLimits(config)
	if err != nil || limits.PasswordConcurrency != 8 || limits.RateLimitEntries != 4096 {
		t.Fatalf("configured authentication limits = %+v, %v", limits, err)
	}
	t.Setenv(rateKey, "0")
	if _, err := authLimits(config); err == nil {
		t.Fatal("accepted a zero-size rate limiter")
	}
}

func TestResendConfig(t *testing.T) {
	const endpointKey = "SHINONA_RESEND_API_URL"
	const timeoutKey = "SHINONA_RESEND_TIMEOUT_SECONDS"
	unsetTestEnv(t, endpointKey)
	unsetTestEnv(t, timeoutKey)
	if got, err := resendConfig(nil); err != nil || got.Endpoint != "https://api.resend.com/emails" || got.Timeout != 8*time.Second {
		t.Fatalf("default Resend config = %+v, %v", got, err)
	}
	config := environment{endpointKey: "https://mailer.example/send", timeoutKey: "21"}
	if got, err := resendConfig(config); err != nil || got.Endpoint != config[endpointKey] || got.Timeout != 21*time.Second {
		t.Fatalf("configured Resend config = %+v, %v", got, err)
	}
	for _, endpoint := range []string{"http://mailer.example/send", "https://localhost/send", "https://169.254.169.254/send",
		"https://user:secret@mailer.example/send", "https://mailer.example/send?", "https://mailer.example/send#fragment"} {
		config[endpointKey] = endpoint
		if _, err := resendConfig(config); err == nil || !strings.Contains(err.Error(), endpointKey) {
			t.Errorf("invalid Resend endpoint %q returned %v", endpoint, err)
		}
	}
	delete(config, endpointKey)
	t.Setenv(timeoutKey, "61")
	if _, err := resendConfig(config); err == nil || !strings.Contains(err.Error(), timeoutKey) {
		t.Fatalf("invalid Resend timeout returned %v", err)
	}
}

func TestWebOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin string
		valid  bool
	}{
		{"http://127.0.0.1:5174", true},
		{"http://localhost:5174", true},
		{"http://192.168.50.10:5174", true},
		{"http://[fd00::1]:5174", true},
		{"https://shinona.example.test", true},
		{"http://0.0.0.0:5174", false},
		{"http://8.8.8.8:5174", false},
		{"http://shinona.example.test", false},
		{"http://192.168.50.10:5174/settings", false},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			if _, err := webOrigin(tc.origin); (err == nil) != tc.valid {
				t.Fatalf("webOrigin(%q): %v, want valid %t", tc.origin, err, tc.valid)
			}
		})
	}
}
