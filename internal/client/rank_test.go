package client

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/montjeffrey/compute-pool/internal/proto"
)

func cand(name string, idle float64, running, max int, accepting bool) Candidate {
	return Candidate{Name: name, Host: name, Info: proto.Info{
		IdleCPUs: idle, Running: running, MaxJobs: max, Accepting: accepting,
	}}
}

func names(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func TestUnreachableAgent(t *testing.T) {
	// Grab a free port, then close it so nothing is listening there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()

	c := New()
	c.Port, _ = strconv.Atoi(port)
	if _, err := c.Info(context.Background(), "127.0.0.1"); !errors.Is(err, ErrUnreachable) {
		t.Errorf("Info: err = %v, want ErrUnreachable", err)
	}
	_, err = c.Run(context.Background(), "127.0.0.1", proto.RunRequest{Argv: []string{"x"}}, Output{Stdout: io.Discard, Stderr: io.Discard})
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("Run: err = %v, want ErrUnreachable", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Info(ctx, "127.0.0.1"); errors.Is(err, ErrUnreachable) {
		t.Error("a canceled request was reported as unreachable")
	}
}

func TestFileSinkDistrustsAgent(t *testing.T) {
	dir := t.TempDir()
	s := newFileSink(dir)
	if err := s.write("out/a.txt", []byte("one ")); err != nil {
		t.Fatal(err)
	}
	if err := s.write("out/a.txt", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := s.write("out/b.txt", []byte("b")); err != nil {
		t.Fatal(err)
	}
	if err := s.write("out/a.txt", []byte("again")); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("re-sent file: err = %v", err)
	}
	for _, bad := range []string{"../escape", "C:/x", "nul"} {
		if err := s.write(bad, []byte("x")); err == nil {
			t.Errorf("unsafe path %q accepted", bad)
		}
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "out", "a.txt")); string(got) != "one two" {
		t.Errorf("a.txt = %q", got)
	}
	if !reflect.DeepEqual(s.order, []string{"out/a.txt", "out/b.txt"}) {
		t.Errorf("order = %v", s.order)
	}

	unrequested := newFileSink("")
	if err := unrequested.write("x", nil); err == nil {
		t.Error("files accepted without an output directory")
	}
}

func TestFileSinkRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.Symlink(target, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	s := newFileSink(dir)
	defer s.close()
	if err := s.write("link.txt", []byte("x")); err == nil {
		t.Error("wrote through a symbolic link")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("the symlink target was created")
	}
}

func TestRank(t *testing.T) {
	tests := []struct {
		name string
		in   []Candidate
		want []string
	}{
		{
			name: "most idle CPUs first",
			in:   []Candidate{cand("laptop", 12, 0, 2, true), cand("desktop", 18, 0, 10, true)},
			want: []string{"desktop", "laptop"},
		},
		{
			name: "skips devices not accepting or full",
			in: []Candidate{
				cand("on-battery", 16, 0, 2, false),
				cand("full", 10, 2, 2, true),
				cand("ok", 4, 0, 2, true),
			},
			want: []string{"ok"},
		},
		{
			name: "equal idle prefers lower slot use, then name",
			in: []Candidate{
				cand("b", 8, 1, 2, true),
				cand("c", 8, 0, 2, true),
				cand("a", 8, 0, 2, true),
			},
			want: []string{"a", "c", "b"},
		},
		{
			name: "nothing available",
			in:   []Candidate{cand("x", 8, 0, 1, false)},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := names(Rank(tt.in)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Rank = %v, want %v", got, tt.want)
			}
		})
	}
}
