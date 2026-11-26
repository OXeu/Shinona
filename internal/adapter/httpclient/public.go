// Package httpclient provides network policies for user-configured endpoints.
package httpclient

import (
	"context"
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

var globalIPv6 = netip.MustParsePrefix("2000::/3")

// Skip special-purpose ranges even if Go considers them global unicast.
// Translation and tunneling ranges can route to a different address family.
var nonPublicNetworks = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

var ErrPrivateEndpoint = errors.New("the endpoint must resolve to a public network address")
var ErrInvalidEndpoint = errors.New("the endpoint must be an absolute HTTP or HTTPS URL without credentials")

// CheckEndpoint rejects private literal addresses before persisting a URL.
// The dialer also checks each resolved address before opening a connection.
func CheckEndpoint(raw string) error {
	target, err := endpointURL(raw)
	if err != nil {
		return err
	}
	return CheckHost(target.Hostname())
}

func endpointURL(raw string) (*url.URL, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if (target.Scheme != "https" && target.Scheme != "http") || target.User != nil || target.Hostname() == "" {
		return nil, ErrInvalidEndpoint
	}
	return target, nil
}

// CheckHost applies the same literal and hostname rules to non-HTTP transports.
func CheckHost(host string) error {
	host = strings.ToLower(strings.TrimRight(host, "."))
	if host == "" {
		return ErrInvalidEndpoint
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return ErrPrivateEndpoint
	}
	if ip, err := netip.ParseAddr(host); err == nil && !publicIP(ip) {
		return ErrPrivateEndpoint
	}
	return nil
}

func Public() *http.Client {
	return &http.Client{Transport: PublicTransport(30*time.Second, false), CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// ModelPolicy permits private addresses for the operator's exact HTTPS origins.
type ModelPolicy struct {
	origins map[string]bool
}

func ParseModelPolicy(allowed string) (ModelPolicy, error) {
	origins := make(map[string]bool)
	for _, raw := range strings.Fields(strings.ReplaceAll(allowed, ",", " ")) {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return ModelPolicy{}, errors.New("trusted model origins must be HTTPS origins without a path, credentials, query, or fragment")
		}
		origins[httpsAuthority(u)] = true
	}
	return ModelPolicy{origins: origins}, nil
}

func (p ModelPolicy) Client() *http.Client {
	client := Public()
	if len(p.origins) > 0 {
		trusted := http.DefaultTransport.(*http.Transport).Clone()
		trusted.Proxy = nil
		client.Transport = modelTransport{public: client.Transport, trusted: trusted, policy: p}
	}
	return client
}

type modelTransport struct {
	public, trusted http.RoundTripper
	policy          ModelPolicy
}

func (t modelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.policy.trusts(req.URL) {
		return t.trusted.RoundTrip(req)
	}
	return t.public.RoundTrip(req)
}

func (p ModelPolicy) trusts(u *url.URL) bool {
	return u.Scheme == "https" && u.User == nil && p.origins[httpsAuthority(u)]
}

func (p ModelPolicy) CheckEndpoint(raw string) error {
	target, err := endpointURL(raw)
	if err != nil {
		return err
	}
	if p.trusts(target) {
		return nil
	}
	return CheckHost(target.Hostname())
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
	transport.DialContext = publicDialer(timeout, loopback).DialContext
	return transport
}

func DialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	return publicDialer(0, false).DialContext(ctx, network, address)
}

func publicDialer(timeout time.Duration, loopback bool) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: func(_, address string, _ syscall.RawConn) error {
		return checkAddress(address, loopback)
	}}
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
	return ErrPrivateEndpoint
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && (!ip.Is6() || globalIPv6.Contains(ip)) &&
		!slices.ContainsFunc(nonPublicNetworks[:], func(network netip.Prefix) bool {
			return network.Contains(ip)
		})
}
