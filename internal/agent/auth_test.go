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
