// Package tsutil reads identity and peer information from the local Tailscale
// daemon through the tailscale CLI.
//
// Shelling out keeps the binary free of the (large) Tailscale Go module and
// works on every platform where Tailscale is installed.
package tsutil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// WhoIs identifies the owner of a tailnet address.
type WhoIs struct {
	NodeName  string // short node name, e.g. "laptop"
	LoginName string // owning user, e.g. "someone@github"
	OS        string
}

// Peer is one device on the tailnet.
type Peer struct {
	Name    string // short node name, e.g. "laptop"
	DNSName string // e.g. "laptop.example.ts.net"
	IPv4    string
	OS      string
	Online  bool
	Self    bool
	UserID  int64 // owning user; all tagged devices share one pseudo-user
	Tagged  bool  // owned by ACL tags rather than a person
}

// Status is the local view of the tailnet.
type Status struct {
	Self       Peer
	SelfLogin  string
	SelfTagged bool
	Peers      []Peer // excludes Self
}

// All returns Self followed by every peer.
func (s Status) All() []Peer {
	return append([]Peer{s.Self}, s.Peers...)
}

// IsOwned reports whether p belongs to the same person as this device. Jobs,
// shipped files and environment values must only ever go to such devices: a
// device shared into the tailnet, or owned by another user or by tags, could
// pose as an idle agent and receive them.
func (s Status) IsOwned(p Peer) bool {
	return p.Self || (!s.SelfTagged && !p.Tagged && p.UserID == s.Self.UserID)
}

// Owned returns Self followed by every peer this device's owner also owns.
func (s Status) Owned() []Peer {
	var out []Peer
	for _, p := range s.All() {
		if s.IsOwned(p) {
			out = append(out, p)
		}
	}
	return out
}

// Resolve finds a device by short name, full DNS name or Tailscale IP.
func (s Status) Resolve(name string) (Peer, error) {
	short := normalizeName(name)
	full := strings.ToLower(strings.TrimSuffix(name, "."))
	for _, p := range s.All() {
		if p.IPv4 == name || strings.ToLower(p.DNSName) == full || (!strings.Contains(full, ".") && p.Name == short) {
			return p, nil
		}
	}
	return Peer{}, fmt.Errorf("no device named %q on this tailnet", name)
}

// LookupWhoIs asks tailscaled who owns ip.
func LookupWhoIs(ctx context.Context, ip string) (WhoIs, error) {
	if net.ParseIP(ip) == nil {
		return WhoIs{}, fmt.Errorf("not an IP address: %q", ip)
	}
	out, err := run(ctx, "whois", "--json", ip)
	if err != nil {
		return WhoIs{}, err
	}
	return parseWhoIs(out)
}

// GetStatus returns the local tailnet status.
func GetStatus(ctx context.Context) (Status, error) {
	out, err := run(ctx, "status", "--json")
	if err != nil {
		return Status{}, err
	}
	return parseStatus(out)
}

func parseWhoIs(b []byte) (WhoIs, error) {
	var raw struct {
		Node struct {
			Name         string
			ComputedName string
			Hostinfo     struct{ OS string }
		}
		UserProfile struct{ LoginName string }
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return WhoIs{}, fmt.Errorf("parse tailscale whois: %w", err)
	}
	name := raw.Node.ComputedName
	if name == "" {
		name = raw.Node.Name
	}
	w := WhoIs{
		NodeName:  normalizeName(name),
		LoginName: raw.UserProfile.LoginName,
		OS:        raw.Node.Hostinfo.OS,
	}
	if w.NodeName == "" || w.LoginName == "" {
		return WhoIs{}, errors.New("tailscale whois returned no node or user")
	}
	return w, nil
}

type rawPeer struct {
	DNSName      string
	HostName     string
	OS           string
	UserID       int64
	TailscaleIPs []string
	Online       bool
	Tags         []string
}

func parseStatus(b []byte) (Status, error) {
	var raw struct {
		BackendState string
		Self         rawPeer
		Peer         map[string]rawPeer
		User         map[string]struct{ LoginName string }
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Status{}, fmt.Errorf("parse tailscale status: %w", err)
	}
	if raw.BackendState != "" && raw.BackendState != "Running" {
		return Status{}, fmt.Errorf("tailscale is not connected (state %s)", raw.BackendState)
	}
	st := Status{
		Self:       convertPeer(raw.Self, true),
		SelfLogin:  raw.User[strconv.FormatInt(raw.Self.UserID, 10)].LoginName,
		SelfTagged: len(raw.Self.Tags) > 0,
	}
	if st.Self.IPv4 == "" {
		return Status{}, errors.New("this device has no Tailscale IPv4 address")
	}
	for _, p := range raw.Peer {
		st.Peers = append(st.Peers, convertPeer(p, false))
	}
	return st, nil
}

func convertPeer(p rawPeer, self bool) Peer {
	name := p.DNSName
	if name == "" {
		name = p.HostName
	}
	out := Peer{
		Name:    normalizeName(name),
		DNSName: strings.TrimSuffix(p.DNSName, "."),
		OS:      p.OS,
		Online:  p.Online || self,
		Self:    self,
		UserID:  p.UserID,
		Tagged:  len(p.Tags) > 0,
	}
	for _, ip := range p.TailscaleIPs {
		if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
			out.IPv4 = ip
			break
		}
	}
	return out
}

// normalizeName reduces "Laptop.example.ts.net." to "laptop".
func normalizeName(name string) string {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	return name
}

func run(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := binary()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func binary() (string, error) {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p, nil
	}
	switch runtime.GOOS {
	case "windows":
		p := filepath.Join(os.Getenv("ProgramFiles"), "Tailscale", "tailscale.exe")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	case "darwin":
		p := "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("tailscale CLI not found; install Tailscale and sign in")
}
