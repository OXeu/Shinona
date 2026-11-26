package httpserver

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestOAuthFailureOmitsProviderSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	logger.Warn("OAuth failure", "failure", OAuthFailure(&oauth2.RetrieveError{
		Response:         &http.Response{StatusCode: http.StatusBadRequest},
		Body:             []byte("secret response"),
		ErrorCode:        "secret provider code",
		ErrorDescription: "secret description",
	}))
	logger.Warn("OAuth network failure", "failure", OAuthFailure(&url.Error{Op: "Post",
		URL: "https://provider.example/token?code=secret-query", Err: errors.New("secret transport details")}))
	if log := output.String(); !strings.Contains(log, `"status":400`) || !strings.Contains(log, `"type":"request"`) || strings.Contains(log, "secret") {
		t.Fatalf("unexpected OAuth diagnostics: %s", log)
	}
}
