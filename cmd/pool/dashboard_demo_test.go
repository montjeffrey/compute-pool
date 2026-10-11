package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

// demoState is a made-up pool used for screenshots and for trying the
// dashboard without any devices. Nothing in it comes from a real tailnet.
func demoState(now time.Time) dashState {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	results := []probeResult{
		{peer: tsutil.Peer{Name: "desktop", Self: true}, info: proto.Info{
			OS: "windows", Arch: "amd64", LogicalCPUs: 20, MaxJobs: 10, Running: 3,
			CPUBusyPct: 34, IdleCPUs: 13.2, BatteryPct: -1, Accepting: true, Version: "0.1.0",
		}},
		{peer: tsutil.Peer{Name: "workstation"}, info: proto.Info{
			OS: "linux", Arch: "amd64", LogicalCPUs: 32, MaxJobs: 16, Running: 2,
			CPUBusyPct: 21, IdleCPUs: 25.3, BatteryPct: -1, Accepting: true, Version: "0.1.0",
		}},
		{peer: tsutil.Peer{Name: "mini-pc"}, info: proto.Info{
			OS: "linux", Arch: "arm64", LogicalCPUs: 8, MaxJobs: 4,
			CPUBusyPct: 81, IdleCPUs: 1.5, BatteryPct: -1, Reason: "cpu busy (81%)", Version: "0.1.0",
		}},
		{peer: tsutil.Peer{Name: "laptop"}, info: proto.Info{
			OS: "windows", Arch: "amd64", LogicalCPUs: 16, MaxJobs: 2,
			CPUBusyPct: 9, IdleCPUs: 14.6, HasBattery: true, OnBattery: true, BatteryPct: 64,
			Reason: "on battery", Version: "0.1.0",
		}},
		{peer: tsutil.Peer{Name: "old-laptop", OS: "linux"}, err: context.DeadlineExceeded},
	}
	job := func(id, cmd string, started time.Duration, ms int64, code int, errMsg string, running bool) proto.JobRecord {
		return proto.JobRecord{ID: id, From: "desktop", Command: cmd, Started: ago(started),
			Running: running, DurationMS: ms, ExitCode: code, Error: errMsg}
	}
	jobs := map[string]proto.Jobs{
		"desktop": {
			Running: []proto.JobRecord{
				job("d-41", "python render.py --scene city.blend --frame 0412", 9*time.Second, 0, 0, "", true),
				job("d-42", "python render.py --scene city.blend --frame 0413", 4*time.Second, 0, 0, "", true),
				job("d-43", "python render.py --scene city.blend --frame 0414", 1*time.Second, 0, 0, "", true),
			},
			Recent: []proto.JobRecord{
				job("d-40", "python render.py --scene city.blend --frame 0411", 21*time.Second, 11840, 0, "", false),
				job("d-38", "go test ./...", 3*time.Minute, 48210, 1, "", false),
			},
		},
		"workstation": {
			Running: []proto.JobRecord{
				job("w-17", "ffmpeg -i clip-07.mov -c:v libx265 clip-07.mkv", 31*time.Second, 0, 0, "", true),
				job("w-18", "ffmpeg -i clip-08.mov -c:v libx265 clip-08.mkv", 12*time.Second, 0, 0, "", true),
			},
			Recent: []proto.JobRecord{
				job("w-16", "ffmpeg -i clip-06.mov -c:v libx265 clip-06.mkv", 75*time.Second, 41230, 0, "", false),
				job("w-12", "cargo build --release", 6*time.Minute, 95500, 0, "", false),
			},
		},
		"mini-pc": {
			Recent: []proto.JobRecord{
				job("m-9", "python backtest.py --year 2024", 2*time.Minute, 64020, 0, "", false),
				job("m-8", "python backtest.py --year 2023", 9*time.Minute, 300000, 124, "job timed out after 300s", false),
			},
		},
	}
	return buildState(results, jobs, now)
}

// TestDemoDashboard serves the dashboard with demoState until interrupted:
//
//	POOL_DEMO_DASHBOARD=127.0.0.1:7472 go test ./cmd/pool -run TestDemoDashboard -timeout 0
func TestDemoDashboard(t *testing.T) {
	addr := os.Getenv("POOL_DEMO_DASHBOARD")
	if addr == "" {
		t.Skip("set POOL_DEMO_DASHBOARD=host:port to serve the demo dashboard")
	}
	d := &dashboard{ttl: time.Second, gather: func(context.Context) (dashState, error) {
		return demoState(time.Now()), nil
	}}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("demo dashboard at http://%s", ln.Addr())
	_ = http.Serve(ln, d.handler())
}

func TestDemoStateIsConsistent(t *testing.T) {
	s := demoState(time.Now())
	running := 0
	for _, d := range s.Devices {
		if d.Info != nil {
			running += d.Info.Running
		}
	}
	if len(s.Devices) != 5 || s.Ready != 2 || len(s.Running) != running {
		t.Errorf("demo state: %d devices, %d ready, %d running jobs listed vs %d counted",
			len(s.Devices), s.Ready, len(s.Running), running)
	}
}
