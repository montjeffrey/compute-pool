package tsutil

import "testing"

// Fixtures mirror the shape of real `tailscale ... --json` output with
// made-up names, addresses and IDs.

const whoisJSON = `{
  "Node": {
    "Name": "laptop.example-tailnet.ts.net.",
    "ComputedName": "laptop",
    "Addresses": ["100.64.0.2/32"],
    "Hostinfo": {"OS": "windows", "Hostname": "LAPTOP"}
  },
  "UserProfile": {"ID": 42, "LoginName": "alice@github", "DisplayName": "Alice"}
}`

const statusJSON = `{
  "BackendState": "Running",
  "Self": {
    "HostName": "DESKTOP", "DNSName": "desktop.example-tailnet.ts.net.", "OS": "windows",
    "UserID": 42, "TailscaleIPs": ["100.64.0.1", "fd7a:115c:a1e0::1"], "Online": true
  },
  "Peer": {
    "nodekey:aaa": {
      "HostName": "LAPTOP", "DNSName": "laptop.example-tailnet.ts.net.", "OS": "windows",
      "UserID": 42, "TailscaleIPs": ["fd7a:115c:a1e0::2", "100.64.0.2"], "Online": true
    },
    "nodekey:bbb": {
      "HostName": "build-box", "DNSName": "build-box.example-tailnet.ts.net.", "OS": "linux",
      "UserID": 42, "TailscaleIPs": ["100.64.0.3"], "Online": false
    }
  },
  "User": {"42": {"ID": 42, "LoginName": "alice@github"}}
}`

func TestParseWhoIs(t *testing.T) {
	w, err := parseWhoIs([]byte(whoisJSON))
	if err != nil {
		t.Fatal(err)
	}
	want := WhoIs{NodeName: "laptop", LoginName: "alice@github", OS: "windows"}
	if w != want {
		t.Errorf("got %+v, want %+v", w, want)
	}
}

func TestParseWhoIsFallsBackToName(t *testing.T) {
	w, err := parseWhoIs([]byte(`{"Node":{"Name":"Laptop.x.ts.net."},"UserProfile":{"LoginName":"a@b"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if w.NodeName != "laptop" {
		t.Errorf("NodeName = %q, want laptop", w.NodeName)
	}
}

func TestParseWhoIsRejectsIncomplete(t *testing.T) {
	for _, in := range []string{`{}`, `{"Node":{"ComputedName":"x"}}`, `not json`} {
		if _, err := parseWhoIs([]byte(in)); err == nil {
			t.Errorf("parseWhoIs(%s) succeeded, want error", in)
		}
	}
}

func TestParseStatus(t *testing.T) {
	st, err := parseStatus([]byte(statusJSON))
	if err != nil {
		t.Fatal(err)
	}
	if st.Self.Name != "desktop" || st.Self.IPv4 != "100.64.0.1" || !st.Self.Self || !st.Self.Online {
		t.Errorf("Self = %+v", st.Self)
	}
	if st.SelfLogin != "alice@github" {
		t.Errorf("SelfLogin = %q", st.SelfLogin)
	}
	if len(st.Peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(st.Peers))
	}

	laptop, err := st.Resolve("LAPTOP")
	if err != nil {
		t.Fatal(err)
	}
	if laptop.IPv4 != "100.64.0.2" { // IPv4 picked even when listed after IPv6
		t.Errorf("laptop IPv4 = %q", laptop.IPv4)
	}
	box, err := st.Resolve("build-box.example-tailnet.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	if box.Online {
		t.Error("build-box should be offline")
	}
	if p, err := st.Resolve("100.64.0.1"); err != nil || !p.Self {
		t.Errorf("Resolve(self IP) = %+v, %v", p, err)
	}
	if _, err := st.Resolve("nope"); err == nil {
		t.Error("Resolve(nope) succeeded")
	}
	if _, err := st.Resolve("laptop.other-tailnet.ts.net"); err == nil {
		t.Error("Resolve matched a DNS name from another tailnet")
	}
}

func TestParseStatusNotRunning(t *testing.T) {
	if _, err := parseStatus([]byte(`{"BackendState":"Stopped"}`)); err == nil {
		t.Error("want error when tailscale is stopped")
	}
}
