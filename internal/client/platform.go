package client

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/montjeffrey/compute-pool/internal/proto"
)

// Platform is the operating system, and optionally the architecture, that a
// job needs. The zero value matches every device.
type Platform struct {
	OS   string // GOOS value such as "windows", "linux" or "darwin"; empty = any
	Arch string // GOARCH value such as "amd64" or "arm64"; empty = any
}

// ParsePlatform reads an --os value: "same" (this device's operating system),
// "any", an operating system such as "linux", or
// os/arch such as "linux/arm64". Case is ignored, and common spellings such
// as "macos", "win", "x86_64" and "aarch64" are read as their Go names.
func ParsePlatform(in string) (Platform, error) {
	s := strings.ToLower(strings.TrimSpace(in))
	switch s {
	case "any":
		return Platform{}, nil
	case "same":
		return Platform{OS: runtime.GOOS}, nil
	}
	goos, arch, hasArch := strings.Cut(s, "/")
	if !validName(goos) || (hasArch && !validName(arch)) || goos == "any" || goos == "same" {
		return Platform{}, fmt.Errorf(`want "same", "any", an operating system such as linux, or os/arch such as linux/arm64; got %q`, in)
	}
	if alias, ok := osAliases[goos]; ok {
		goos = alias
	}
	if alias, ok := archAliases[arch]; ok {
		arch = alias
	}
	return Platform{OS: goos, Arch: arch}, nil
}

var (
	osAliases   = map[string]string{"macos": "darwin", "mac": "darwin", "osx": "darwin", "win": "windows"}
	archAliases = map[string]string{"x86_64": "amd64", "x64": "amd64", "aarch64": "arm64"}
)

// validName reports whether s could be a GOOS or GOARCH value (or an alias):
// lowercase letters, digits and underscores.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// Matches reports whether a device whose agent sent info can run the job. A
// device that does not report its system matches only when any system will do.
func (p Platform) Matches(info proto.Info) bool {
	if p.OS != "" && !strings.EqualFold(p.OS, info.OS) {
		return false
	}
	return p.Arch == "" || strings.EqualFold(p.Arch, info.Arch)
}

func (p Platform) String() string {
	switch {
	case p.OS == "":
		return "any"
	case p.Arch == "":
		return p.OS
	}
	return p.OS + "/" + p.Arch
}
