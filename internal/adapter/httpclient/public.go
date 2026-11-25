// Package httpclient provides network policies for user-configured endpoints.
package httpclient

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"
)

var nonPublicNetworks = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2002::/16"),
}

var errPrivateEndpoint = errors.New("the endpoint must resolve to a public network address")

// CheckEndpoint rejects private literal addresses before persisting a URL.
// The dialer also checks each resolved address before opening a connection.
func CheckEndpoint(raw string) error {
	target, err := url.Parse(raw)
	if err != nil {
		return err
	}
	host := strings.ToLower(strings.TrimRight(target.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errPrivateEndpoint
	}
	if ip, err := netip.ParseAddr(host); err == nil && !publicIP(ip) {
		return errPrivateEndpoint
	}
	return nil
}

func Public() *http.Client {
	return &http.Client{Transport: PublicTransport(30*time.Second, false), CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// ForModels permits private addresses for the operator's exact HTTPS origins.
// Requests to other origins continue through the public address policy.
func ForModels(allowed string) (*http.Client, error) {
	client := Public()
	origins := make(map[string]bool)
	for _, raw := range strings.Fields(strings.ReplaceAll(allowed, ",", " ")) {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return nil, errors.New("trusted model origins must be HTTPS origins without a path, credentials, query, or fragment")
		}
		origins[httpsAuthority(u)] = true
	}
	if len(origins) > 0 {
		trusted := http.DefaultTransport.(*http.Transport).Clone()
		trusted.Proxy = nil
		client.Transport = modelTransport{public: client.Transport, trusted: trusted, origins: origins}
	}
	return client, nil
}

type modelTransport struct {
	public, trusted http.RoundTripper
	origins         map[string]bool
}

func (t modelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.trusts(req.URL) {
		return t.trusted.RoundTrip(req)
	}
	return t.public.RoundTrip(req)
}

func (t modelTransport) trusts(u *url.URL) bool {
	return u.Scheme == "https" && t.origins[httpsAuthority(u)]
}

func CheckModelEndpoint(client *http.Client, raw string) error {
	if transport, ok := client.Transport.(modelTransport); ok {
		if u, err := url.Parse(raw); err == nil && transport.trusts(u) {
			return nil
		}
	}
	return CheckEndpoint(raw)
}

func httpsAuthority(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// PublicTransport optionally permits loopback for development adapters.
func PublicTransport(timeout time.Duration, loopback bool) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: timeout, Control: func(_, address string, _ syscall.RawConn) error {
		return checkAddress(address, loopback)
	}}).DialContext
	return transport
}

func checkAddress(address string, loopback bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if publicIP(ip) || (loopback && ip.Unmap().IsLoopback()) {
		return nil
	}
	return errPrivateEndpoint
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !slices.ContainsFunc(nonPublicNetworks, func(network netip.Prefix) bool {
		return network.Contains(ip)
	})
}
