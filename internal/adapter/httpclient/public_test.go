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
		"0.1.2.3:443", "192.0.2.1:443", "198.51.100.1:443", "[::10.0.0.1]:443",
		"[fec0::1]:443", "[100::1]:443", "[2001:db8::1]:443", "[3fff::1]:443",
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

func TestEndpointMustHaveHTTPAuthority(t *testing.T) {
	for _, raw := range []string{"", "/relative", "//example.com", "ssh://example.com", "https://user@example.com"} {
		if err := CheckEndpoint(raw); !errors.Is(err, ErrInvalidEndpoint) {
			t.Errorf("invalid endpoint %q returned %v", raw, err)
		}
	}
}

type wrappedModelTransport struct{ http.RoundTripper }

func TestTrustedModelOriginAllowsOnlyMatchingHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	policy, err := ParseModelPolicy(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := policy.Client()
	transport := client.Transport.(modelTransport)
	transport.trusted.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	client.Transport = wrappedModelTransport{client.Transport}
	if err := policy.CheckEndpoint(server.URL + "/v1"); err != nil {
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
		if err := policy.CheckEndpoint(blocked); !errors.Is(err, ErrPrivateEndpoint) {
			t.Errorf("different model origin %q returned %v", blocked, err)
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, blocked, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		if !errors.Is(err, ErrPrivateEndpoint) {
			t.Errorf("wrapped transport allowed a request to %q: %v", blocked, err)
		}
	}
	credentials := strings.Replace(server.URL, "https://", "https://user@", 1)
	if err := policy.CheckEndpoint(credentials); !errors.Is(err, ErrInvalidEndpoint) {
		t.Errorf("trusted origin accepted embedded credentials: %v", err)
	}
}
