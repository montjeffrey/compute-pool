package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

func fakeAuthorizer(whois map[string]tsutil.WhoIs, calls *int) *TailnetAuthorizer {
	a := NewTailnetAuthorizer("alice@github", []string{"Desktop", " laptop ", ""})
	a.lookup = func(_ context.Context, ip string) (tsutil.WhoIs, error) {
		*calls++
		w, ok := whois[ip]
		if !ok {
			return tsutil.WhoIs{}, errors.New("no such peer")
		}
		return w, nil
	}
	return a
}

func TestTailnetAuthorizer(t *testing.T) {
	var calls int
	a := fakeAuthorizer(map[string]tsutil.WhoIs{
		"100.64.0.1": {NodeName: "desktop", LoginName: "alice@github"},
		"100.64.0.2": {NodeName: "laptop", LoginName: "Alice@GitHub"}, // login compare ignores case
		"100.64.0.3": {NodeName: "sales-box", LoginName: "alice@github"},
		"100.64.0.4": {NodeName: "laptop", LoginName: "mallory@github"}, // shared-in node, same name
	}, &calls)

	tests := []struct {
		ip        string
		wantErr   bool
		forbidden bool
	}{
		{"100.64.0.1", false, false},
		{"100.64.0.2", false, false},
		{"100.64.0.3", true, true}, // right owner, not on allow list
		{"100.64.0.4", true, true}, // on allow list by name, wrong owner
		{"100.64.0.9", true, false},
	}
	for _, tt := range tests {
		_, err := a.Authorize(context.Background(), tt.ip)
		if (err != nil) != tt.wantErr {
			t.Errorf("Authorize(%s) err = %v, wantErr %v", tt.ip, err, tt.wantErr)
		}
		if errors.Is(err, ErrForbidden) != tt.forbidden {
			t.Errorf("Authorize(%s) forbidden = %v, want %v", tt.ip, errors.Is(err, ErrForbidden), tt.forbidden)
		}
	}
	if got := a.Allowed(); len(got) != 2 || got[0] != "desktop" || got[1] != "laptop" {
		t.Errorf("Allowed() = %v", got)
	}
}

func TestNonTailnetAddressesRejectedWithoutLookup(t *testing.T) {
	var calls int
	a := fakeAuthorizer(map[string]tsutil.WhoIs{
		"100.64.0.1": {NodeName: "desktop", LoginName: "alice@github"},
	}, &calls)
	for _, ip := range []string{"192.168.1.10", "127.0.0.1", "8.8.8.8", "::1", "fe80::1", "not-an-ip"} {
		if _, err := a.Authorize(context.Background(), ip); !errors.Is(err, ErrForbidden) {
			t.Errorf("Authorize(%s) err = %v, want forbidden", ip, err)
		}
	}
	if calls != 0 {
		t.Errorf("tailscale whois ran %d times for non-tailnet addresses", calls)
	}
	// An IPv4-mapped IPv6 form of a tailnet address is still a tailnet address.
	if !inTailnetRange("::ffff:100.64.0.1") || !inTailnetRange("fd7a:115c:a1e0::5") {
		t.Error("tailnet address forms not recognized")
	}
}

func TestOwnerComparisonIsNotUnicodeFolding(t *testing.T) {
	var calls int
	a := fakeAuthorizer(map[string]tsutil.WhoIs{
		// U+212A KELVIN SIGN folds to "k" under strings.EqualFold.
		"100.64.0.7": {NodeName: "desktop", LoginName: "aliceK@github"},
	}, &calls)
	a.owner = "alicek@github"
	if _, err := a.Authorize(context.Background(), "100.64.0.7"); !errors.Is(err, ErrForbidden) {
		t.Errorf("look-alike login accepted: %v", err)
	}
}

func TestTailnetAuthorizerCachesSuccessfulLookups(t *testing.T) {
	var calls int
	a := fakeAuthorizer(map[string]tsutil.WhoIs{
		"100.64.0.1": {NodeName: "desktop", LoginName: "alice@github"},
	}, &calls)
	for i := 0; i < 3; i++ {
		if _, err := a.Authorize(context.Background(), "100.64.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		_, _ = a.Authorize(context.Background(), "100.64.0.9")
	}
	if calls != 4 { // 1 cached success + 3 uncached failures
		t.Errorf("lookup called %d times, want 4", calls)
	}
}

// waitLookups waits for the lookups running in the background to finish.
func waitLookups(a *TailnetAuthorizer) {
	a.mu.Lock()
	calls := make([]*whoisCall, 0, len(a.inflight))
	for _, c := range a.inflight {
		calls = append(calls, c)
	}
	a.mu.Unlock()
	for _, c := range calls {
		<-c.done
	}
}

// blockingAuthorizer's lookups wait for release, and count themselves.
func blockingAuthorizer(release <-chan struct{}, calls *atomic.Int32, lookupErr *error) *TailnetAuthorizer {
	a := NewTailnetAuthorizer("alice@github", []string{"desktop"})
	a.lookup = func(ctx context.Context, ip string) (tsutil.WhoIs, error) {
		calls.Add(1)
		<-release
		if err := ctx.Err(); err != nil {
			*lookupErr = err
			return tsutil.WhoIs{}, err
		}
		return tsutil.WhoIs{NodeName: "desktop", LoginName: "alice@github"}, nil
	}
	return a
}

// A busy device's status probe gives up before tailscale whois answers. The
// lookup must still finish and be cached, or the device is refused forever.
func TestWhoisOutlivesCallerThatGivesUp(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	var lookupErr error
	a := blockingAuthorizer(release, &calls, &lookupErr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller has already given up
	_, err := a.Authorize(ctx, "100.64.0.1")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrForbidden) {
		t.Fatalf("Authorize err = %v, want an identify error from the canceled request", err)
	}
	close(release)
	waitLookups(a)
	if lookupErr != nil {
		t.Fatalf("lookup was canceled with its caller: %v", lookupErr)
	}
	if _, err := a.Authorize(context.Background(), "100.64.0.1"); err != nil {
		t.Fatalf("next request: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("lookup ran %d times, want 1 (the next request should use the cache)", n)
	}
}

func TestConcurrentRequestsShareOneLookup(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	var lookupErr error
	a := blockingAuthorizer(release, &calls, &lookupErr)

	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = a.Authorize(context.Background(), "100.64.0.1")
		}()
	}
	close(release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("request %d: %v", i, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("lookup ran %d times for 5 concurrent requests, want 1", n)
	}
}

// fakeClock is a settable clock for the authorizer.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestCachedLookupRefreshedBeforeExpiry(t *testing.T) {
	var calls int
	a := fakeAuthorizer(map[string]tsutil.WhoIs{
		"100.64.0.1": {NodeName: "desktop", LoginName: "alice@github"},
	}, &calls)
	clock := &fakeClock{time.Unix(1_000_000, 0)}
	a.now = clock.now
	t0 := clock.t

	authorize := func(at time.Duration) {
		t.Helper()
		clock.t = t0.Add(at)
		if _, err := a.Authorize(context.Background(), "100.64.0.1"); err != nil {
			t.Fatalf("at %v: %v", at, err)
		}
		waitLookups(a)
	}
	authorize(0)                // looks up
	authorize(30 * time.Second) // fresh: cached
	if calls != 1 {
		t.Fatalf("lookup ran %d times in the first half of the TTL, want 1", calls)
	}
	authorize(50 * time.Second) // last quarter: cached, refreshed behind it
	if calls != 2 {
		t.Fatalf("lookup ran %d times, want a background refresh", calls)
	}
	authorize(70 * time.Second) // past the first lookup's TTL, within the refresh's
	if calls != 2 {
		t.Errorf("lookup ran %d times, want the refreshed entry used", calls)
	}
}

// A device that stops being allowed loses access within the TTL even while
// it keeps polling: a failed refresh is not cached and does not extend the
// old entry.
func TestRemovedDeviceLosesAccessWithinTTL(t *testing.T) {
	var calls int
	whois := map[string]tsutil.WhoIs{
		"100.64.0.1": {NodeName: "desktop", LoginName: "alice@github"},
	}
	a := fakeAuthorizer(whois, &calls)
	clock := &fakeClock{time.Unix(1_000_000, 0)}
	a.now = clock.now
	t0 := clock.t

	if _, err := a.Authorize(context.Background(), "100.64.0.1"); err != nil {
		t.Fatal(err)
	}
	delete(whois, "100.64.0.1") // removed from the tailnet: whois now fails
	clock.t = t0.Add(50 * time.Second)
	if _, err := a.Authorize(context.Background(), "100.64.0.1"); err != nil {
		t.Fatalf("within the TTL: %v", err)
	}
	waitLookups(a) // the failed refresh
	clock.t = t0.Add(61 * time.Second)
	if _, err := a.Authorize(context.Background(), "100.64.0.1"); err == nil {
		t.Fatal("removed device still allowed after the TTL")
	}
}
