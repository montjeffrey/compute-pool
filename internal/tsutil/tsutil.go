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
	NodeName  string // short node name, e.g. "jm-c"
	LoginName string // owning user, e.g. "someone@github"
	OS        string
}

// Peer is one device on the tailnet.
type Peer struct {
	Name    string // short node name, e.g. "jm-c"
	DNSName string // e.g. "jm-c.tailnet.ts.net"
	IPv4    string
	OS      string
	Online  bool
	Self    bool
}

// Status is the local view of the tailnet.
type Status struct {
	Self      Peer
	SelfLogin string
	Peers     []Peer // excludes Self
}

// All returns Self followed by every peer.
func (s Status) All() []Peer {
	return append([]Peer{s.Self}, s.Peers...)
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
		Self:      convertPeer(raw.Self, true),
		SelfLogin: raw.User[strconv.FormatInt(raw.Self.UserID, 10)].LoginName,
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
	}
	for _, ip := range p.TailscaleIPs {
		if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
			out.IPv4 = ip
			break
		}
	}
	return out
}

// normalizeName reduces "JM-C.tailnet.ts.net." to "jm-c".
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
