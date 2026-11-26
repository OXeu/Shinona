package httpauth

import (
	"container/list"
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
	clients map[netip.Addr]*list.Element
	recent  list.List
}

type loginClient struct {
	ip      netip.Addr
	limiter *rate.Limiter
	last    time.Time
}

func (l *loginRate) allow(ip netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ip.Is6() {
		ip = netip.PrefixFrom(ip, 64).Masked().Addr()
	}
	for oldest := l.recent.Back(); oldest != nil && now.Sub(oldest.Value.(*loginClient).last) >= loginAddressIdle; oldest = l.recent.Back() {
		l.remove(oldest)
	}
	element := l.clients[ip]
	if element == nil {
		if l.recent.Len() == loginAddressLimit {
			l.remove(l.recent.Back())
		}
		if l.clients == nil {
			l.clients = make(map[netip.Addr]*list.Element)
		}
		element = l.recent.PushFront(&loginClient{ip: ip, limiter: rate.NewLimiter(rate.Every(time.Second), 5)})
		l.clients[ip] = element
	}
	entry := element.Value.(*loginClient)
	entry.last = now
	l.recent.MoveToFront(element)
	return entry.limiter.AllowN(now, 1)
}

func (l *loginRate) remove(element *list.Element) {
	delete(l.clients, element.Value.(*loginClient).ip)
	l.recent.Remove(element)
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
