package client

import (
	"runtime"
	"strings"
	"testing"

	"github.com/montjeffrey/compute-pool/internal/proto"
)

func TestParsePlatform(t *testing.T) {
	good := map[string]Platform{
		"any":           {},
		"ANY":           {},
		"same":          {OS: runtime.GOOS},
		"linux":         {OS: "linux"},
		"Windows":       {OS: "windows"},
		"macos":         {OS: "darwin"},
		"linux/arm64":   {OS: "linux", Arch: "arm64"},
		" darwin/AMD64": {OS: "darwin", Arch: "amd64"},
		"linux/x86_64":  {OS: "linux", Arch: "amd64"},
		"linux/aarch64": {OS: "linux", Arch: "arm64"},
		"win":           {OS: "windows"},
		"osx/arm64":     {OS: "darwin", Arch: "arm64"},
	}
	for in, want := range good {
		got, err := ParsePlatform(in)
		if err != nil || got != want {
			t.Errorf("ParsePlatform(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/amd64", "linux/", "linux/arm64/v8", "win dows", "same/arm64", "any/amd64"} {
		if p, err := ParsePlatform(bad); err == nil {
			t.Errorf("ParsePlatform(%q) = %+v, want an error", bad, p)
		}
	}
}

func TestParsePlatformEchoesInput(t *testing.T) {
	_, err := ParsePlatform("Win Dows")
	if err == nil || !strings.Contains(err.Error(), `"Win Dows"`) {
		t.Errorf("err = %v, want it to quote the value as typed", err)
	}
}

func TestPlatformMatches(t *testing.T) {
	win := proto.Info{OS: "windows", Arch: "amd64"}
	arm := proto.Info{OS: "linux", Arch: "arm64"}
	unknown := proto.Info{}
	tests := []struct {
		p    Platform
		info proto.Info
		want bool
	}{
		{Platform{}, win, true},
		{Platform{}, unknown, true},
		{Platform{OS: "windows"}, win, true},
		{Platform{OS: "windows"}, arm, false},
		{Platform{OS: "windows"}, unknown, false},
		{Platform{OS: "linux", Arch: "arm64"}, arm, true},
		{Platform{OS: "linux", Arch: "amd64"}, arm, false},
		{Platform{OS: "windows"}, proto.Info{OS: "Windows", Arch: "AMD64"}, true},
		{Platform{OS: "linux", Arch: "arm64"}, proto.Info{OS: "linux"}, false},
	}
	for _, tt := range tests {
		if got := tt.p.Matches(tt.info); got != tt.want {
			t.Errorf("%+v.Matches(%s/%s) = %v, want %v", tt.p, tt.info.OS, tt.info.Arch, got, tt.want)
		}
	}
}

func TestPlatformString(t *testing.T) {
	for p, want := range map[Platform]string{
		{}:                           "any",
		{OS: "linux"}:                "linux",
		{OS: "linux", Arch: "arm64"}: "linux/arm64",
	} {
		if got := p.String(); got != want {
			t.Errorf("%+v.String() = %q, want %q", p, got, want)
		}
	}
}
