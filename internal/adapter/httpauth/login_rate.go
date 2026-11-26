package httpauth

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/OXeu/shinona/internal/responsecache"
)

type requestRate[K comparable] struct {
	mu      sync.Mutex
	buckets responsecache.Store[K, *rate.Limiter]
}

func newRequestRate[K comparable](entries int) requestRate[K] {
	return requestRate[K]{buckets: responsecache.NewStore[K, *rate.Limiter](entries, entries)}
}

func (l *requestRate[K]) allow(key K, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	limiter, found := l.buckets.Get(key)
	if !found {
		limiter = rate.NewLimiter(rate.Every(time.Second), 5)
		l.buckets.Put(key, limiter, 1, nil)
	}
	return limiter.AllowN(now, 1)
}

func loginAddress(r *http.Request, proxies []netip.Prefix) netip.Addr {
	address := loginIP(r, proxies)
	if address.Is6() {
		return netip.PrefixFrom(address, 64).Masked().Addr()
	}
	return address
}

func loginIP(r *http.Request, proxies []netip.Prefix) netip.Addr {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	address := peer.Addr().Unmap().WithZone("")
	trusted := func(ip netip.Addr) bool {
		return slices.ContainsFunc(proxies, func(prefix netip.Prefix) bool { return prefix.Contains(ip) })
	}
	if !trusted(address) {
		return address
	}
	// Work back from the trusted peer so client-supplied entries to the left
	// of the first untrusted hop cannot select the rate-limit bucket.
	headers := r.Header.Values("X-Forwarded-For")
	for i := len(headers) - 1; i >= 0; i-- {
		addresses := strings.Split(headers[i], ",")
		for j := len(addresses) - 1; j >= 0; j-- {
			forwarded, err := netip.ParseAddr(strings.TrimSpace(addresses[j]))
			if err != nil {
				return peer.Addr().Unmap().WithZone("")
			}
			address = forwarded.Unmap().WithZone("")
			if !trusted(address) {
				return address
			}
		}
	}
	return address
}
