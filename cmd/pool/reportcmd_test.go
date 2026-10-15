package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/montjeffrey/compute-pool/internal/agent"
	"github.com/montjeffrey/compute-pool/internal/client"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

// sampleLog is a short agent log: 40 one-second tasks on a 4-slot agent.
func sampleLog() string {
	t := time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local)
	const layout = "2006/01/02 15:04:05.000000"
	var b strings.Builder
	fmt.Fprintf(&b, "%s pool agent 0.1.5 on node-a; owner someone@example; allowed devices: node-a; max jobs 4\n", t.Format(layout))
	for i := 0; i < 40; i++ {
		s := t.Add(time.Second + time.Duration(i/4)*time.Second)
		fmt.Fprintf(&b, "%s job node-a-x-%d from node-b: %q (shell=false)\n", s.Format(layout), i, []string{"bench.exe", "-n", "100", strconv.Itoa(i)})
		fmt.Fprintf(&b, "%s job node-a-x-%d done: exit=0 in 1s\n", s.Add(time.Second).Format(layout), i)
	}
	return b.String()
}

func writeSampleLog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.log")
	if err := os.WriteFile(path, []byte(sampleLog()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReportFromLogFile(t *testing.T) {
	path := writeSampleLog(t)
	var out, errb bytes.Buffer
	if code := run([]string{"report", "--log", path}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"=== node-a:", "bench.exe -n 100 {}", "1. Speed by jobs at once", "4. Reliability", "40 jobs: 40 (100.0%) succeeded"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestReportJSON(t *testing.T) {
	path := writeSampleLog(t)
	var out, errb bytes.Buffer
	if code := run([]string{"report", "--json", "--min-jobs", "10", "--log", path}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var rep struct {
		Devices []struct {
			Node      string
			Workloads []struct{ Jobs int }
		}
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Devices) != 1 || rep.Devices[0].Node != "node-a" || len(rep.Devices[0].Workloads) != 1 || rep.Devices[0].Workloads[0].Jobs != 40 {
		t.Errorf("report = %+v", rep)
	}
}

func TestReportFlagErrors(t *testing.T) {
	for _, args := range [][]string{
		{"report", "--log", "x", "--nodes", "a"},
		{"report", "extra"},
		{"report", "--log", filepath.Join(t.TempDir(), "missing.log")},
	} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code == 0 {
			t.Errorf("%v: want an error", args)
		}
	}
}

type allowAny struct{}

func (allowAny) Authorize(context.Context, string) (agent.Identity, error) {
	return agent.Identity{Node: "node-b"}, nil
}

// serve starts h on 127.0.0.1 and returns a client pointed at it.
func serve(t *testing.T, h http.Handler) *client.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	c := client.New()
	c.Port, _ = strconv.Atoi(port)
	return c
}

func TestFetchDeviceFromAgent(t *testing.T) {
	a, err := agent.New(agent.Config{NodeName: "node-a", MaxJobs: 1, WorkDir: t.TempDir(), Auth: allowAny{},
		Logger: log.New(io.Discard, "", 0), LogPath: writeSampleLog(t)})
	if err != nil {
		t.Fatal(err)
	}
	c := serve(t, a.Handler())
	capa := &proto.Capacity{Kernel: "k", Levels: []proto.Level{{Jobs: 1, Rate: 1}}}
	d, problem := fetchDevice(context.Background(), c, probeResult{peer: tsutil.Peer{Name: "laptop", IPv4: "127.0.0.1"}, info: proto.Info{Capacity: capa}})
	if problem != "" || d == nil {
		t.Fatalf("problem %q", problem)
	}
	if d.Log.Node != "node-a" || len(d.Log.Jobs) != 40 || d.Capacity != capa {
		t.Errorf("device = %+v", d)
	}
}

func TestFetchDeviceOldAgent(t *testing.T) {
	c := serve(t, http.NotFoundHandler()) // an agent without /v1/log
	_, problem := fetchDevice(context.Background(), c, probeResult{peer: tsutil.Peer{Name: "laptop", IPv4: "127.0.0.1"}, info: proto.Info{Version: "0.1.4"}})
	if !strings.Contains(problem, "agent 0.1.4 is too old to share its log") {
		t.Errorf("problem = %q", problem)
	}
}

func TestFetchDeviceSelfFallsBackToLocalLog(t *testing.T) {
	path := writeSampleLog(t)
	old := localAgentLog
	localAgentLog = func() string { return path }
	t.Cleanup(func() { localAgentLog = old })
	useCapacityFile(t) // no measurement saved

	// This device's agent isn't running: the installed log is read instead.
	r := probeResult{peer: tsutil.Peer{Name: "node-a", Self: true, IPv4: "127.0.0.1"}, err: errors.New("connection refused")}
	d, problem := fetchDevice(context.Background(), client.New(), r)
	if problem != "" || d == nil || len(d.Log.Jobs) != 40 {
		t.Fatalf("device %+v, problem %q", d, problem)
	}
	// Another device whose agent is down is skipped.
	r.peer.Self = false
	if _, problem := fetchDevice(context.Background(), client.New(), r); !strings.Contains(problem, "skipped") {
		t.Errorf("problem = %q", problem)
	}
}

func TestReportFromFilesAttachesRemoteMeasurement(t *testing.T) {
	path := writeSampleLog(t) // node-a, not this device
	capa := &proto.Capacity{Kernel: "k", Levels: []proto.Level{{Jobs: 1, Rate: 1}, {Jobs: 4, Rate: 4}}}
	calls := 0
	devs, err := reportFromFiles([]string{path, path}, func() map[string]*proto.Capacity {
		calls++
		return map[string]*proto.Capacity{"node-a": capa}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 || devs[0].Capacity != capa || devs[1].Capacity != capa {
		t.Errorf("devices = %+v", devs)
	}
	if calls != 1 {
		t.Errorf("devices were asked %d times, want once", calls)
	}
	// Without Tailscale there is simply no measurement.
	devs, err = reportFromFiles([]string{path}, func() map[string]*proto.Capacity { return nil })
	if err != nil || devs[0].Capacity != nil {
		t.Errorf("devices = %+v, %v", devs, err)
	}
}
