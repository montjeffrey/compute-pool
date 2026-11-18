package main

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/montjeffrey/compute-pool/internal/client"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

func platformResults() []probeResult {
	ready := func(name, goos string, idle float64) probeResult {
		return probeResult{peer: tsutil.Peer{Name: name}, info: proto.Info{
			OS: goos, Arch: "amd64", IdleCPUs: idle, MaxJobs: 2, Accepting: true, Reason: "ready",
		}}
	}
	return []probeResult{
		ready("desktop", "windows", 4),
		ready("server", "linux", 30), // most idle, so it would win on load alone
		{peer: tsutil.Peer{Name: "gone"}, err: client.ErrUnreachable},
	}
}

func targetNames(cs []client.Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func TestChooseTargetsSkipsOtherSystems(t *testing.T) {
	got, err := chooseTargets(platformResults(), client.Platform{OS: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(targetNames(got), []string{"desktop"}) {
		t.Errorf("targets = %v, want only the Windows device", targetNames(got))
	}

	got, err = chooseTargets(platformResults(), client.Platform{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(targetNames(got), []string{"server", "desktop"}) {
		t.Errorf("with any system, targets = %v, want most idle first", targetNames(got))
	}
}

func TestChooseTargetsExplainsSystemMismatch(t *testing.T) {
	_, err := chooseTargets(platformResults(), client.Platform{OS: "darwin"})
	if err == nil {
		t.Fatal("a job for darwin was placed on a device that does not run it")
	}
	msg := err.Error()
	for _, want := range []string{"desktop: runs windows/amd64", "server: runs linux/amd64", "gone: ", "--os any"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "low on free memory") {
		t.Errorf("error %q blames memory for a system mismatch", msg)
	}
}

func TestNoDeviceErrorWithoutMismatchHasNoOSHint(t *testing.T) {
	results := []probeResult{{peer: tsutil.Peer{Name: "laptop"}, info: proto.Info{
		OS: "windows", MaxJobs: 2, Accepting: false, Reason: "on battery",
	}}}
	_, err := chooseTargets(results, client.Platform{OS: "windows"})
	if err == nil {
		t.Fatal("a paused device was chosen")
	}
	if msg := err.Error(); strings.Contains(msg, "--os") || !strings.Contains(msg, "laptop: on battery") {
		t.Errorf("error = %q", msg)
	}
}

func TestChooseTargetsArchAndUnknownSystem(t *testing.T) {
	results := []probeResult{
		{peer: tsutil.Peer{Name: "pi"}, info: proto.Info{OS: "linux", Arch: "arm64", MaxJobs: 2, Accepting: true}},
		{peer: tsutil.Peer{Name: "old"}, info: proto.Info{MaxJobs: 2, Accepting: true}},
	}
	_, err := chooseTargets(results, client.Platform{OS: "linux", Arch: "amd64"})
	if err == nil {
		t.Fatal("an amd64 job was placed on an arm64 device")
	}
	for _, want := range []string{"pi: runs linux/arm64", "old: did not report its system"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestNoDeviceErrorPausedAndOtherSystem(t *testing.T) {
	results := []probeResult{
		{peer: tsutil.Peer{Name: "laptop"}, info: proto.Info{OS: "windows", MaxJobs: 2, Accepting: false, Reason: "low memory"}},
		{peer: tsutil.Peer{Name: "server"}, info: proto.Info{OS: "linux", Arch: "amd64", MaxJobs: 2, Accepting: true}},
	}
	_, err := chooseTargets(results, client.Platform{OS: "windows"})
	if err == nil {
		t.Fatal("expected no target")
	}
	for _, want := range []string{"laptop: low memory", "server: runs linux/amd64", "low on free memory", "--os any"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestSkippedNote(t *testing.T) {
	names := otherSystems(platformResults(), client.Platform{OS: "windows"})
	if !reflect.DeepEqual(names, []string{"server"}) {
		t.Errorf("otherSystems = %v, want [server]", names)
	}
	if note := skippedNote(names, client.Platform{OS: "windows"}); !strings.Contains(note, "server") || !strings.Contains(note, "not running windows") {
		t.Errorf("note = %q", note)
	}
	if note := skippedNote(nil, client.Platform{OS: "windows"}); note != "" {
		t.Errorf("note with nothing skipped = %q", note)
	}
}

func TestOSFlagRefusedWithNamedDevices(t *testing.T) {
	code, err := cmdRun(context.Background(), []string{"--node", "laptop", "--os", "linux", "--", "echo"}, io.Discard, io.Discard)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--node auto") {
		t.Errorf("run: code %d, err %v", code, err)
	}
	code, err = cmdMap(context.Background(), []string{"--nodes", "laptop", "--os", "linux", "--range", "1..2", "--", "echo"}, io.Discard, io.Discard)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--nodes") {
		t.Errorf("map: code %d, err %v", code, err)
	}
	if code, err := cmdRun(context.Background(), []string{"--os", "win dows", "--", "echo"}, io.Discard, io.Discard); code != 2 || err == nil {
		t.Errorf("bad --os: code %d, err %v", code, err)
	}
}
