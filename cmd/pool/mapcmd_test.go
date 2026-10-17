package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/montjeffrey/compute-pool/internal/client"
	"github.com/montjeffrey/compute-pool/internal/fanout"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

func items(ts []fanout.Task) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Item)
	}
	return out
}

func TestBuildTasksRange(t *testing.T) {
	ts, local, err := buildTasks("3..6", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := items(ts); !reflect.DeepEqual(got, []string{"3", "4", "5", "6"}) || ts[0].Index != 1 || len(local) != 0 {
		t.Errorf("tasks = %+v", ts)
	}
	for _, bad := range []string{"5..1", "1-5", "a..b", "1..200001"} {
		if _, _, err := buildTasks(bad, "", nil, nil); err == nil {
			t.Errorf("buildTasks(%q) succeeded", bad)
		}
	}
}

func TestBuildTasksItems(t *testing.T) {
	ts, _, err := buildTasks("", "-", nil, strings.NewReader("alpha\r\n\n  beta  \ngamma"))
	if err != nil {
		t.Fatal(err)
	}
	if got := items(ts); !reflect.DeepEqual(got, []string{"alpha", "beta", "gamma"}) {
		t.Errorf("items = %v", got)
	}
}

func TestBuildTasksInputs(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"b.csv", "a.csv", "skip.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "dir.csv"), 0o755); err != nil {
		t.Fatal(err)
	}
	pat := filepath.Join(dir, "*.csv")
	ts, local, err := buildTasks("", "", []string{pat, pat}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := items(ts); !reflect.DeepEqual(got, []string{"a.csv", "b.csv"}) {
		t.Errorf("items = %v (directories and duplicates must be skipped)", got)
	}
	if local[1] != filepath.Join(dir, "a.csv") || local[2] != filepath.Join(dir, "b.csv") {
		t.Errorf("local = %v", local)
	}
}

func TestBuildTasksNeedsExactlyOneSource(t *testing.T) {
	if _, _, err := buildTasks("", "", nil, nil); err == nil {
		t.Error("no source accepted")
	}
	if _, _, err := buildTasks("1..2", "-", nil, strings.NewReader("x")); err == nil {
		t.Error("two sources accepted")
	}
	if _, _, err := buildTasks("", "", []string{filepath.Join(t.TempDir(), "*.none")}, nil); err == nil {
		t.Error("empty input list accepted")
	}
}

func TestSubstitute(t *testing.T) {
	got := substitute([]string{"tool", "--in={}", "--n", "{#}", "{}.out"}, fanout.Task{Index: 7, Item: "x.csv"})
	want := []string{"tool", "--in=x.csv", "--n", "7", "x.csv.out"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLoadShipped(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "tool.py"), []byte("print(1)"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "lib", "sub"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "lib", "a.py"), []byte("a"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "lib", "sub", "b.py"), []byte("b"), 0o644))

	files, err := loadShipped([]string{filepath.Join(dir, "tool.py"), filepath.Join(dir, "lib")})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = string(f.Data)
	}
	want := map[string]string{"tool.py": "print(1)", "lib/a.py": "a", "lib/sub/b.py": "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("shipped = %v, want %v", got, want)
	}
	if _, err := loadShipped([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Error("missing file accepted")
	}
}

func TestResultPrinterKeepOrder(t *testing.T) {
	var out, errb bytes.Buffer
	p := newResultPrinter(&out, &errb, false, true)
	for _, i := range []int{3, 1, 2} {
		p.add(fanout.Result{Task: fanout.Task{Index: i}, Outcome: fanout.Outcome{Stdout: []byte{byte('0' + i)}}})
	}
	p.flush()
	if out.String() != "123" {
		t.Errorf("output = %q, want 123", out.String())
	}
	if errb.Len() != 0 {
		t.Errorf("unexpected stderr %q for successful tasks", errb.String())
	}
}

func TestOpenLogFileRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "agent.log")
	f, err := openLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("first\n"))
	f.Close()
	if f, err = openLogFile(path); err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("second\n"))
	f.Close()
	if got, _ := os.ReadFile(path); string(got) != "first\nsecond\n" {
		t.Errorf("small log not appended: %q", got)
	}

	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxLogBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err = openLogFile(path); err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("fresh\n"))
	f.Close()
	if got, _ := os.ReadFile(path); string(got) != "fresh\n" {
		t.Errorf("log not rotated: %d bytes", len(got))
	}
	if info, err := os.Stat(path + ".1"); err != nil || info.Size() != maxLogBytes+1 {
		t.Errorf("rotated file: %v", err)
	}
}

func TestRotatingLogRotatesWhileRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	l, err := newRotatingLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 12; i++ { // 12 MiB through a 10 MiB limit
		if _, err := l.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := os.Stat(path)
	old, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal("no rotated file")
	}
	if cur.Size() > maxLogBytes || old.Size() > maxLogBytes {
		t.Errorf("sizes after rotation: current %d, old %d", cur.Size(), old.Size())
	}
}

// A reader holding the log open (pool report) makes the rename fail on
// Windows. Rotation must then wait, not empty the file in place.
func TestRotatingLogKeepsLinesWhileRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	l, err := newRotatingLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	line := append(bytes.Repeat([]byte("x"), 1<<20-1), '\n')
	for i := 0; i < 9; i++ {
		if _, err := l.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // past the limit while the reader is open
		if _, err := l.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	reader.Close()
	if _, err := l.Write([]byte("last\n")); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, p := range []string{path, path + ".1"} {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	if want := int64(12*len(line) + len("last\n")); total != want {
		t.Errorf("log and rotated log hold %d bytes, want %d: lines were lost", total, want)
	}
}

func TestAgentHosts(t *testing.T) {
	got := agentHosts("100.64.0.1:7470", tsutil.Peer{IPv4: "100.64.0.1", DNSName: "desktop.example.ts.net", Name: "desktop"})
	want := []string{"100.64.0.1:7470", "100.64.0.1:7470", "desktop.example.ts.net:7470", "desktop:7470"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestCappedBuffer(t *testing.T) {
	var b cappedBuffer
	b.Write(bytes.Repeat([]byte("a"), maxTaskOutput-2))
	b.Write([]byte("bcde"))
	b.Write([]byte("fg"))
	out := b.Bytes()
	if !bytes.HasPrefix(out, bytes.Repeat([]byte("a"), maxTaskOutput-2)) || !bytes.Contains(out, []byte("abc\n[pool: 4 more bytes")) {
		t.Errorf("tail = %q", out[len(out)-50:])
	}
	var small cappedBuffer
	small.Write([]byte("ok"))
	if string(small.Bytes()) != "ok" {
		t.Errorf("small = %q", small.Bytes())
	}
}

func TestResolveOwned(t *testing.T) {
	st := tsutil.Status{
		Self: tsutil.Peer{Name: "desktop", IPv4: "100.64.0.1", Self: true, UserID: 1},
		Peers: []tsutil.Peer{
			{Name: "laptop", IPv4: "100.64.0.2", UserID: 1},
			{Name: "friends-pc", IPv4: "100.64.0.3", UserID: 2},
			{Name: "ci", IPv4: "100.64.0.4", UserID: 1, Tagged: true},
		},
	}
	if p, err := resolveOwned(st, "laptop"); err != nil || p.IPv4 != "100.64.0.2" {
		t.Errorf("laptop: %+v %v", p, err)
	}
	for _, n := range []string{"friends-pc", "100.64.0.3", "ci"} {
		if _, err := resolveOwned(st, n); err == nil || !strings.Contains(err.Error(), "not one of your devices") {
			t.Errorf("%s: err = %v", n, err)
		}
	}
	if _, err := resolveOwned(st, "192.168.1.5"); err == nil {
		t.Error("a non-tailnet IP was accepted")
	}
}

func TestCheckListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if reason := checkListener(context.Background(), addr, ""); reason != "" {
		t.Errorf("healthy listener reported: %s", reason)
	}
	ln.Close()
	if reason := checkListener(context.Background(), addr, ""); reason == "" {
		t.Error("closed listener reported healthy")
	}
}

func TestBuildState(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	results := []probeResult{
		{peer: tsutil.Peer{Name: "zeta", OS: "linux"}, err: errors.New("dial: refused")},
		{peer: tsutil.Peer{Name: "laptop"}, info: proto.Info{LogicalCPUs: 16, IdleCPUs: 14, Accepting: false, Reason: "on battery"}},
		{peer: tsutil.Peer{Name: "desktop", Self: true}, info: proto.Info{LogicalCPUs: 20, IdleCPUs: 17.5, Accepting: true}},
		{peer: tsutil.Peer{Name: "office"}, err: fmt.Errorf("wrapped: %w", client.ErrForbidden)},
		{peer: tsutil.Peer{Name: "busybox"}, info: proto.Info{LogicalCPUs: 4, Running: 2, MaxJobs: 2, Accepting: false, Reason: "all 2 job slots in use"}},
	}
	jobs := map[string]proto.Jobs{
		"desktop": {
			Running: []proto.JobRecord{{ID: "d2", Started: now.Add(-time.Second)}},
			Recent:  []proto.JobRecord{{ID: "d1", Started: now.Add(-3 * time.Minute)}},
		},
		"laptop": {Recent: []proto.JobRecord{{ID: "l1", Started: now.Add(-time.Minute)}}},
	}
	s := buildState(results, jobs, now)

	var order, statuses []string
	for _, d := range s.Devices {
		order = append(order, d.Name)
		statuses = append(statuses, d.Status)
	}
	if !reflect.DeepEqual(order, []string{"busybox", "desktop", "laptop", "office", "zeta"}) {
		t.Errorf("device order = %v", order)
	}
	if !reflect.DeepEqual(statuses, []string{"busy", "ready", "paused", "refused", "no-agent"}) {
		t.Errorf("statuses = %v", statuses)
	}
	if s.Ready != 1 || s.TotalCPUs != 40 || s.IdleCPUs != 17.5 {
		t.Errorf("totals: ready=%d cpus=%d idle=%v", s.Ready, s.TotalCPUs, s.IdleCPUs)
	}
	if len(s.Running) != 1 || s.Running[0].Device != "desktop" {
		t.Errorf("running = %+v", s.Running)
	}
	if len(s.Recent) != 2 || s.Recent[0].ID != "l1" || s.Recent[1].ID != "d1" {
		t.Errorf("recent not newest first: %+v", s.Recent)
	}
}

func TestDashboardHandler(t *testing.T) {
	calls := 0
	d := &dashboard{ttl: time.Minute, gather: func(context.Context) (dashState, error) {
		calls++
		return dashState{Ready: 2, Devices: []dashDevice{{Name: "desktop", Status: "ready"}}}, nil
	}}
	h := requireHost([]string{"127.0.0.1:7471", "localhost:7471"}, d.handler())

	get := func(host, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	page := get("127.0.0.1:7471", "/")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "<title>Compute pool</title>") {
		t.Errorf("page: %d", page.Code)
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	api := get("localhost:7471", "/api/state")
	var s dashState
	if api.Code != 200 || json.Unmarshal(api.Body.Bytes(), &s) != nil || s.Ready != 2 || s.Devices[0].Name != "desktop" {
		t.Errorf("api: %d %s", api.Code, api.Body.String())
	}
	get("localhost:7471", "/api/state")
	if calls != 1 {
		t.Errorf("gather called %d times; the snapshot should be cached", calls)
	}
	if rebind := get("attacker.example:7471", "/api/state"); rebind.Code != 403 {
		t.Errorf("foreign Host header got %d, want 403", rebind.Code)
	}
}

func TestResultPrinterReportsFailures(t *testing.T) {
	var out, errb bytes.Buffer
	p := newResultPrinter(&out, &errb, false, false)
	p.add(fanout.Result{Task: fanout.Task{Index: 2, Item: "b"}, Device: "laptop", Outcome: fanout.Outcome{ExitCode: 4}})
	if !strings.Contains(errb.String(), "task 2 (b) on laptop: exit 4") {
		t.Errorf("stderr = %q", errb.String())
	}
}
