package httpauth

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	loginAddressLimit = 8192
	loginAddressIdle  = time.Minute
)

type loginRate struct {
	mu      sync.Mutex
	clients map[netip.Addr]loginClient
	sweepAt time.Time
}

type loginClient struct {
	limiter *rate.Limiter
	last    time.Time
}

func (l *loginRate) allow(ip netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !now.Before(l.sweepAt) {
		for address, entry := range l.clients {
			if now.Sub(entry.last) >= loginAddressIdle {
				delete(l.clients, address)
			}
		}
		l.sweepAt = now.Add(loginAddressIdle)
	}
	entry, ok := l.clients[ip]
	if !ok {
		if len(l.clients) >= loginAddressLimit {
			return false
		}
		if l.clients == nil {
			l.clients = make(map[netip.Addr]loginClient)
		}
		entry.limiter = rate.NewLimiter(rate.Every(time.Second), 5)
	}
	entry.last = now
	l.clients[ip] = entry
	return entry.limiter.AllowN(now, 1)
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
