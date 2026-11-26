package email

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/auth"
)

type resendTransportFunc func(*http.Request) (*http.Response, error)

func (f resendTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResendUsesConfiguredDeliveryAndVerificationExpiry(t *testing.T) {
	const endpoint = "https://mailer.example/send"
	const timeout = 21 * time.Second
	const sender = "Shinona <accounts@example.com>"
	verification := auth.EmailVerification{
		RequestID: "request-id", Address: "person@example.com", Code: "01234567",
		ExpiresAt: time.Date(2030, time.January, 2, 12, 34, 56, 0, time.FixedZone("UTC+8", 8*60*60)),
	}
	client := &http.Client{Transport: resendTransportFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != endpoint || req.Header.Get("Authorization") != "Bearer test-key" ||
			req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Idempotency-Key") != "shinona/request-id" {
			t.Error("unexpected Resend request")
		}
		if deadline, ok := req.Context().Deadline(); !ok || time.Until(deadline) > timeout || time.Until(deadline) <= 0 {
			t.Errorf("unexpected Resend deadline %v", deadline)
		}
		defer req.Body.Close()
		var payload struct {
			From    string
			To      []string
			Subject string
			Text    string
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			return nil, err
		}
		if payload.From != sender || len(payload.To) != 1 || payload.To[0] != verification.Address || payload.Subject == "" {
			t.Errorf("unexpected sender, recipient, or subject: %+v", payload)
		}
		if strings.Count(payload.Text, verification.Code) != 2 || strings.Count(payload.Text, "2030-01-02 04:34:56 UTC") != 2 {
			t.Errorf("unexpected body: %q", payload.Text)
		}
		return httptest.NewRecorder().Result(), nil
	})}
	mailer := Resend{APIKey: "test-key", From: sender, Client: client, Config: ResendConfig{Endpoint: endpoint, Timeout: timeout}}
	if err := mailer.SendVerification(t.Context(), verification); err != nil {
		t.Fatal(err)
	}
}
