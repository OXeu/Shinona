// Package email delivers account verification messages.
package email

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/OXeu/shinona/internal/auth"
)

type ResendConfig struct {
	Endpoint string
	Timeout  time.Duration
}

func DefaultResendConfig() ResendConfig {
	return ResendConfig{Endpoint: "https://api.resend.com/emails", Timeout: 8 * time.Second}
}

//go:embed verification.txt
var verificationText string

var verificationTemplate = template.Must(template.New("verification").Parse(verificationText))

type Resend struct {
	APIKey string
	From   string
	Client *http.Client
	Config ResendConfig
}

func (r Resend) SendVerification(ctx context.Context, verification auth.EmailVerification) error {
	config := r.Config
	defaults := DefaultResendConfig()
	if config.Timeout <= 0 {
		config.Timeout = defaults.Timeout
	}
	if config.Endpoint == "" {
		config.Endpoint = defaults.Endpoint
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()
	var message strings.Builder
	if err := verificationTemplate.Execute(&message, verification); err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		Text    string   `json:"text"`
	}{r.From, []string{verification.Address}, "Shinona email verification", message.String()})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "shinona/"+verification.RequestID)
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send verification email: %w", err)
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("verification email service: HTTP %d", response.StatusCode)
	}
	return err
}
