package httpclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccountEndpointsRejectPrivateDialAddresses(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1:443", "10.0.0.1:443", "169.254.169.254:443", "100.64.0.1:443",
		"[::ffff:127.0.0.1]:443", "[fd00::1]:443", "[64:ff9b::a00:1]:443",
	} {
		if err := checkAddress(address, false); !errors.Is(err, ErrPrivateEndpoint) {
			t.Errorf("private address %s returned %v", address, err)
		}
	}
	for _, address := range []string{"8.8.8.8:443", "[2606:4700::1111]:443"} {
		if err := checkAddress(address, false); err != nil {
			t.Errorf("public address %s returned %v", address, err)
		}
	}
	if err := checkAddress("127.0.0.1:443", true); err != nil {
		t.Errorf("loopback developer transport returned %v", err)
	}
}

func TestTrustedModelOriginAllowsOnlyMatchingHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := ForModels(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(modelTransport)
	transport.trusted.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	if err := CheckModelEndpoint(client, server.URL+"/v1"); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("trusted endpoint status: %d", response.StatusCode)
	}
	for _, blocked := range []string{"https://127.0.0.1:1/v1", strings.Replace(server.URL, "https://", "http://", 1)} {
		if err := CheckModelEndpoint(client, blocked); !errors.Is(err, ErrPrivateEndpoint) {
			t.Errorf("different model origin %q returned %v", blocked, err)
		}
	}
}
