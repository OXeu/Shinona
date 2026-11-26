package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"

	"golang.org/x/oauth2"
)

// OAuthFailure reports safely loggable diagnostics for untrusted provider
// failures. Provider response bodies, descriptions, and request URLs may
// contain credentials.
func OAuthFailure(err error) slog.Value {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return slog.GroupValue(slog.String("type", "deadline"))
	case errors.Is(err, context.Canceled):
		return slog.GroupValue(slog.String("type", "canceled"))
	}
	if failure, ok := errors.AsType[*oauth2.RetrieveError](err); ok && failure.Response != nil {
		return slog.GroupValue(slog.String("type", "provider_response"), slog.Int("status", failure.Response.StatusCode))
	}
	if failure, ok := errors.AsType[*url.Error](err); ok {
		return slog.GroupValue(slog.String("type", "request"), slog.String("cause_type", fmt.Sprintf("%T", failure.Err)))
	}
	return slog.GroupValue(slog.String("type", fmt.Sprintf("%T", err)))
}
