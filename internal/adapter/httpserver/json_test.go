package httpserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONBody(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		strict     bool
		valid      bool
		oversized  bool
	}{
		{name: "single value with whitespace", body: "{\"Name\":\"ok\"} \n\t", valid: true},
		{name: "unknown fields allowed", body: `{"Name":"ok","Extra":1}`, valid: true},
		{name: "unknown fields disallowed", body: `{"Name":"ok","Extra":1}`, strict: true},
		{name: "multiple values", body: `{"Name":"ok"} {}`},
		{name: "broken trailing value", body: `{"Name":"ok"} {`},
		{name: "oversized trailing whitespace", body: `{"Name":"ok"}` + strings.Repeat(" ", 32), oversized: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var payload struct{ Name string }
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(tt.body))
			decode := DecodeJSON
			if tt.strict {
				decode = DecodeStrictJSON
			}
			err := decode(httptest.NewRecorder(), request, 32, &payload)
			var oversized *http.MaxBytesError
			if (err == nil) != tt.valid || errors.As(err, &oversized) != tt.oversized || (tt.valid && payload.Name != "ok") {
				t.Fatalf("decoded %+v with %v", payload, err)
			}
		})
	}
}
