package agent

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

// ErrForbidden marks a caller that was identified but is not allowed.
var ErrForbidden = errors.New("forbidden")

// Identity is the tailnet device behind a request.
type Identity struct {
	Node  string
	Login string
}

// Authorizer decides whether the device at remoteIP may use the agent.
type Authorizer interface {
	Authorize(ctx context.Context, remoteIP string) (Identity, error)
}

// TailnetAuthorizer admits a caller only when tailscaled reports that its
// address belongs to a node on the allow list owned by the same user as this
// agent. Tailscale's WireGuard keys are what make that report trustworthy:
// a peer cannot send from an address it was not assigned.
type TailnetAuthorizer struct {
	owner  string
	allow  map[string]bool
	lookup func(ctx context.Context, ip string) (tsutil.WhoIs, error)
	ttl    time.Duration
	now    func() time.Time

	mu       sync.Mutex
	cache    map[string]cachedWhoIs // successful lookups only
	inflight map[string]*whoisCall
}

type cachedWhoIs struct {
	who     tsutil.WhoIs
	expires time.Time
}

// whoisCall is one tailscale whois run, shared by every request from that
// address while it runs. done is closed once who and err are set.
type whoisCall struct {
	done chan struct{}
	who  tsutil.WhoIs
	err  error
}

// whoisTimeout bounds one tailscale whois run.
const whoisTimeout = 5 * time.Second

// NewTailnetAuthorizer admits nodes named in allow (short names, any case)
// that belong to owner.
func NewTailnetAuthorizer(owner string, allow []string) *TailnetAuthorizer {
	set := make(map[string]bool, len(allow))
	for _, n := range allow {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			set[n] = true
		}
	}
	return &TailnetAuthorizer{
		owner:    owner,
		allow:    set,
		lookup:   tsutil.LookupWhoIs,
		ttl:      time.Minute,
		now:      time.Now,
		cache:    make(map[string]cachedWhoIs),
		inflight: make(map[string]*whoisCall),
	}
}

// Allowed returns the allow list, sorted.
func (a *TailnetAuthorizer) Allowed() []string {
	out := make([]string, 0, len(a.allow))
	for n := range a.allow {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// asciiEqualFold compares ignoring case for A-Z only. Unicode-aware folding
// (strings.EqualFold, strings.ToLower) also maps look-alikes such as the
// Kelvin sign to "k", which must not make two logins match.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// Tailscale assigns addresses only from these ranges.
var tailnetRanges = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

func inTailnetRange(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range tailnetRanges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Authorize implements Authorizer.
func (a *TailnetAuthorizer) Authorize(ctx context.Context, remoteIP string) (Identity, error) {
	// Anything else is refused before running tailscale, so a scanner on a
	// LAN (possible with --listen) cannot make the agent spawn processes.
	if !inTailnetRange(remoteIP) {
		return Identity{}, fmt.Errorf("%w: %s is not a Tailscale address", ErrForbidden, remoteIP)
	}
	who, err := a.whois(ctx, remoteIP)
	if err != nil {
		return Identity{}, fmt.Errorf("identify %s: %w", remoteIP, err)
	}
	id := Identity{Node: who.NodeName, Login: who.LoginName}
	if !asciiEqualFold(who.LoginName, a.owner) {
		return id, fmt.Errorf("%w: %s belongs to %s, not %s", ErrForbidden, who.NodeName, who.LoginName, a.owner)
	}
	if !a.allow[who.NodeName] {
		return id, fmt.Errorf("%w: node %s is not on the allow list", ErrForbidden, who.NodeName)
	}
	return id, nil
}

// whois returns who is at ip, from the cache while a successful lookup is
// less than ttl old. Only successes are cached, so a device removed from the
// tailnet or the allow list loses access within ttl.
//
// The lookup runs apart from the request: on a busy device tailscale whois
// can take longer than a status probe waits (a second), and killing it when
// the caller gave up meant the cache never filled, so every probe from an
// allowed device was refused. Now the lookup finishes and the next request
// is served from the cache. An entry in the last quarter of its life is
// refreshed in the background, so a device that keeps polling never waits.
func (a *TailnetAuthorizer) whois(ctx context.Context, ip string) (tsutil.WhoIs, error) {
	now := a.now()
	a.mu.Lock()
	c, cached := a.cache[ip]
	cached = cached && now.Before(c.expires)
	call := a.inflight[ip]
	if call == nil && (!cached || c.expires.Sub(now) < a.ttl/4) {
		call = &whoisCall{done: make(chan struct{})}
		a.inflight[ip] = call
		go a.runWhois(ctx, ip, call, now)
	}
	a.mu.Unlock()
	if cached {
		return c.who, nil
	}
	select {
	case <-call.done:
		return call.who, call.err
	case <-ctx.Done():
		return tsutil.WhoIs{}, ctx.Err()
	}
}

// runWhois runs one lookup started at start and caches it if it succeeded.
func (a *TailnetAuthorizer) runWhois(ctx context.Context, ip string, call *whoisCall, start time.Time) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), whoisTimeout)
	defer cancel()
	call.who, call.err = a.lookup(ctx, ip)
	a.mu.Lock()
	delete(a.inflight, ip)
	if call.err == nil {
		// Expiry counts from the start, never extending what a lookup vouches for.
		a.cache[ip] = cachedWhoIs{who: call.who, expires: start.Add(a.ttl)}
		for k, e := range a.cache {
			if !start.Before(e.expires) {
				delete(a.cache, k)
			}
		}
	}
	a.mu.Unlock()
	close(call.done)
}
