package client

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strconv"
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
