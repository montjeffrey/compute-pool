package agent

import (
	"context"
	"errors"
	"fmt"
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

	mu    sync.Mutex
	cache map[string]cachedWhoIs
}

type cachedWhoIs struct {
	who     tsutil.WhoIs
	expires time.Time
}

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
		owner:  owner,
		allow:  set,
		lookup: tsutil.LookupWhoIs,
		ttl:    time.Minute,
		cache:  make(map[string]cachedWhoIs),
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

// Authorize implements Authorizer.
func (a *TailnetAuthorizer) Authorize(ctx context.Context, remoteIP string) (Identity, error) {
	who, err := a.whois(ctx, remoteIP)
	if err != nil {
		return Identity{}, fmt.Errorf("identify %s: %w", remoteIP, err)
	}
	id := Identity{Node: who.NodeName, Login: who.LoginName}
	if !strings.EqualFold(who.LoginName, a.owner) {
		return id, fmt.Errorf("%w: %s belongs to %s, not %s", ErrForbidden, who.NodeName, who.LoginName, a.owner)
	}
	if !a.allow[who.NodeName] {
		return id, fmt.Errorf("%w: node %s is not on the allow list", ErrForbidden, who.NodeName)
	}
	return id, nil
}

func (a *TailnetAuthorizer) whois(ctx context.Context, ip string) (tsutil.WhoIs, error) {
	now := time.Now()
	a.mu.Lock()
	c, ok := a.cache[ip]
	a.mu.Unlock()
	if ok && now.Before(c.expires) {
		return c.who, nil
	}
	who, err := a.lookup(ctx, ip)
	if err != nil {
		return tsutil.WhoIs{}, err
	}
	a.mu.Lock()
	a.cache[ip] = cachedWhoIs{who: who, expires: now.Add(a.ttl)}
	a.mu.Unlock()
	return who, nil
}
