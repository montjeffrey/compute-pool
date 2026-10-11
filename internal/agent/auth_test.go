package agent

import (
	"context"
	"errors"
	"testing"

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
