package agent_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/montjeffrey/compute-pool/internal/agent"
	"github.com/montjeffrey/compute-pool/internal/client"
	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/sysinfo"
)

// TestHelperProcess is not a real test: the agent runs this test binary as
// the "remote" program, choosing behavior with the argument after "--".
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(2)
	}
	switch args[1] {
	case "echo":
		fmt.Fprint(os.Stdout, "hello from job\n")
		fmt.Fprint(os.Stderr, "warning from job\n")
		os.Exit(3)
	case "env":
		fmt.Printf("%v|%s|%s\n", os.Getenv("POOL_JOB_ID") != "", os.Getenv("POOL_NODE"), os.Getenv("FOO"))
	case "pwd":
		wd, _ := os.Getwd()
		fmt.Print(wd)
	case "files":
		in, err := os.ReadFile(filepath.Join("data", "in.txt"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_ = os.MkdirAll(filepath.Join("out", "sub"), 0o755)
		_ = os.WriteFile(filepath.Join("out", "result.txt"), bytes.ToUpper(in), 0o644)
		_ = os.WriteFile(filepath.Join("out", "sub", "nested.txt"), []byte("nested"), 0o644)
		_ = os.WriteFile(filepath.Join("out", "empty.txt"), nil, 0o644)
		_ = os.WriteFile("not-collected.txt", []byte("x"), 0o644)
	case "sameas":
		// Whether this program is the same file as $SAME_AS (a hard link to it).
		self, err1 := os.Executable()
		a, err2 := os.Stat(self)
		b, err3 := os.Stat(os.Getenv("SAME_AS"))
		fmt.Print(err1 == nil && err2 == nil && err3 == nil && os.SameFile(a, b))
	case "alloc":
		// Use 200 MB, touching every page so it is really committed.
		b := make([]byte, 200<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		runtime.KeepAlive(b)
	case "sleep":
		time.Sleep(time.Minute)
	case "spawn":
		// Start a grandchild, then wait; canceling the job must kill both.
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "heartbeat")
		child.Env = os.Environ()
		if err := child.Start(); err != nil {
			os.Exit(1)
		}
		fmt.Println("spawned")
		time.Sleep(time.Minute)
	case "heartbeat":
		deadline := time.Now().Add(30 * time.Second) // never outlive a broken test for long
		for i := 0; time.Now().Before(deadline); i++ {
			_ = os.WriteFile(os.Getenv("HEARTBEAT_FILE"), []byte(strconv.Itoa(i)), 0o600)
			time.Sleep(50 * time.Millisecond)
		}
	}
	os.Exit(0)
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, string) (agent.Identity, error) {
	return agent.Identity{Node: "tester", Login: "tester@example"}, nil
}

type denyAll struct{}

func (denyAll) Authorize(context.Context, string) (agent.Identity, error) {
	return agent.Identity{}, agent.ErrForbidden
}

type harness struct {
	agent    *agent.Agent
	client   *client.Client
	host     string
	workDir  string
	outDir   string // where the client writes collected files
	cacheDir string
}

func newHarness(t *testing.T, maxJobs int, auth agent.Authorizer) *harness {
	return newHarnessWith(t, maxJobs, auth, nil)
}

func newHarnessWith(t *testing.T, maxJobs int, auth agent.Authorizer, tweak func(*agent.Config)) *harness {
	t.Helper()
	workDir := t.TempDir()
	cacheDir := t.TempDir()
	cfg := agent.Config{
		NodeName: "test-node",
		MaxJobs:  maxJobs,
		WorkDir:  workDir,
		Auth:     auth,
		Logger:   log.New(io.Discard, "", 0),
		CacheDir: cacheDir,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	c := client.New()
	c.Port, _ = strconv.Atoi(port)
	return &harness{agent: a, client: c, host: host, workDir: workDir, outDir: t.TempDir(), cacheDir: cacheDir}
}

func helperRequest(t *testing.T, mode string, env map[string]string) proto.RunRequest {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]string{"GO_WANT_HELPER_PROCESS": "1"}
	for k, v := range env {
		all[k] = v
	}
	return proto.RunRequest{Argv: []string{exe, "-test.run=TestHelperProcess", "--", mode}, Env: all}
}

func (h *harness) run(t *testing.T, ctx context.Context, req proto.RunRequest) (client.Result, string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	res, err := h.client.Run(ctx, h.host, req, client.Output{Stdout: &out, Stderr: &errb, Dir: h.outDir})
	return res, out.String(), errb.String(), err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunStreamsOutputAndExitCode(t *testing.T) {
	h := newHarness(t, 2, allowAll{})
	res, out, errOut, err := h.run(t, context.Background(), helperRequest(t, "echo", nil))
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello from job\n" || errOut != "warning from job\n" {
		t.Errorf("stdout=%q stderr=%q", out, errOut)
	}
	if res.ExitCode != 3 || res.Node != "test-node" || res.JobID == "" || res.RemoteError != "" {
		t.Errorf("result = %+v", res)
	}
}

func TestRunSetsEnvironment(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	req := helperRequest(t, "env", map[string]string{"FOO": "bar", "POOL_NODE": "spoofed"})
	_, out, _, err := h.run(t, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "true|test-node|bar" {
		t.Errorf("env output = %q, want true|test-node|bar", out)
	}
}

func TestRunUsesFreshScratchDirAndRemovesIt(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	_, dir, _, err := h.run(t, context.Background(), helperRequest(t, "pwd", nil))
	if err != nil {
		t.Fatal(err)
	}
	// Compare resolved paths: macOS reports /var as /private/var, and
	// Windows may report short names (RUNNER~1). The job folder itself is
	// already deleted, so resolve its parent, which must be the work dir.
	if !strings.HasPrefix(filepath.Base(dir), "job-") {
		t.Fatalf("job ran in %q, want a job-* folder", dir)
	}
	parent, err1 := filepath.EvalSymlinks(filepath.Dir(dir))
	work, err2 := filepath.EvalSymlinks(h.workDir)
	if err1 != nil || err2 != nil || !strings.EqualFold(parent, work) {
		t.Fatalf("job ran in %q, want a subdirectory of %q (%v %v)", dir, h.workDir, err1, err2)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("scratch dir %q still exists after the job (err=%v)", dir, err)
	}
}

func TestRunShell(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	// "&&" means the same thing in cmd.exe and /bin/sh.
	req := proto.RunRequest{Argv: []string{"echo", "first", "&&", "echo", "second"}, Shell: true}
	res, out, _, err := h.run(t, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Errorf("exit=%d out=%q", res.ExitCode, out)
	}
}

func TestRunMissingProgram(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	res, _, _, err := h.run(t, context.Background(), proto.RunRequest{Argv: []string{"no-such-program-7f3a"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 127 || !strings.Contains(res.RemoteError, "no-such-program-7f3a") {
		t.Errorf("result = %+v", res)
	}
}

func TestRunTimeout(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	req := helperRequest(t, "sleep", nil)
	req.TimeoutSec = 1
	start := time.Now()
	res, _, _, err := h.run(t, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 124 || !strings.Contains(res.RemoteError, "timed out") {
		t.Errorf("result = %+v", res)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("timeout took %s", took)
	}
}

func TestCancelKillsWholeProcessTree(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	beat := filepath.Join(t.TempDir(), "heartbeat")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, _, err := h.run(t, ctx, helperRequest(t, "spawn", map[string]string{"HEARTBEAT_FILE": beat}))
		done <- err
	}()
	waitFor(t, "grandchild heartbeat", func() bool { _, err := os.Stat(beat); return err == nil })

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run after cancel: err = %v, want context.Canceled", err)
	}
	waitFor(t, "agent to finish the job", func() bool { return h.agent.Running() == 0 })

	// If the grandchild survived, it keeps rewriting the file.
	time.Sleep(300 * time.Millisecond)
	before, _ := os.ReadFile(beat)
	time.Sleep(500 * time.Millisecond)
	after, _ := os.ReadFile(beat)
	if !bytes.Equal(before, after) {
		t.Errorf("grandchild still running after cancel (heartbeat %s -> %s)", before, after)
	}
}

func TestBusyAgentRejectsExtraJobs(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _, _, _ = h.run(t, ctx, helperRequest(t, "sleep", nil)) }()
	waitFor(t, "first job to start", func() bool { return h.agent.Running() == 1 })

	_, _, _, err := h.run(t, context.Background(), helperRequest(t, "echo", nil))
	if !errors.Is(err, client.ErrBusy) {
		t.Errorf("second job: err = %v, want ErrBusy", err)
	}
	info, err := h.client.Info(context.Background(), h.host)
	if err != nil {
		t.Fatal(err)
	}
	if info.Running != 1 || info.MaxJobs != 1 || info.Node != "test-node" || info.Version != proto.Version {
		t.Errorf("info = %+v", info)
	}
	cancel()
	waitFor(t, "first job to stop", func() bool { return h.agent.Running() == 0 })
}

func TestForbiddenCaller(t *testing.T) {
	h := newHarness(t, 1, denyAll{})
	if _, err := h.client.Info(context.Background(), h.host); !errors.Is(err, client.ErrForbidden) {
		t.Errorf("Info: err = %v, want ErrForbidden", err)
	}
	_, _, _, err := h.run(t, context.Background(), proto.RunRequest{Argv: []string{"whoami"}})
	if !errors.Is(err, client.ErrForbidden) {
		t.Errorf("Run: err = %v, want ErrForbidden", err)
	}
}

func TestShipAndCollectFiles(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	req := helperRequest(t, "files", nil)
	req.Files = []proto.InputFile{{Path: "data/in.txt", Data: []byte("hello pool")}}
	req.Collect = []string{"out"}
	res, _, errOut, err := h.run(t, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.RemoteError != "" {
		t.Fatalf("result = %+v, stderr = %q", res, errOut)
	}
	want := map[string]string{
		"out/result.txt":     "HELLO POOL",
		"out/sub/nested.txt": "nested",
		"out/empty.txt":      "",
	}
	if len(res.Files) != len(want) {
		t.Errorf("collected %v, want %d files", res.Files, len(want))
	}
	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(h.outDir, filepath.FromSlash(rel)))
		if err != nil || string(got) != content {
			t.Errorf("%s = %q, %v; want %q", rel, got, err, content)
		}
	}
	if _, err := os.Stat(filepath.Join(h.outDir, "not-collected.txt")); !os.IsNotExist(err) {
		t.Error("a file outside the collect patterns came back")
	}
}

func TestCollectReportsUnmatchedPatterns(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	req := helperRequest(t, "pwd", nil)
	req.Collect = []string{"*.csv"}
	res, _, _, err := h.run(t, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.RemoteError, "nothing matched *.csv") {
		t.Errorf("result = %+v", res)
	}
}

func TestShippedProgramRunsByName(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	name := "helper"
	if filepath.Ext(exe) == ".exe" {
		name += ".exe"
	}
	h := newHarness(t, 1, allowAll{})
	req := proto.RunRequest{
		// The bare name would fail a PATH lookup; the agent must use the shipped copy.
		Argv:  []string{strings.TrimSuffix(name, ".exe"), "-test.run=TestHelperProcess", "--", "echo"},
		Env:   map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
		Files: []proto.InputFile{{Path: name, Data: data}},
	}
	res, out, _, err := h.run(t, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || out != "hello from job\n" {
		t.Errorf("result = %+v, stdout = %q", res, out)
	}
}

func sha(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// shippedHelper returns a request that ships this test binary, with its hash,
// and runs it by name in the given helper mode.
func shippedHelper(t *testing.T, mode string) (proto.RunRequest, []byte) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	name := "helper"
	if filepath.Ext(exe) == ".exe" {
		name += ".exe"
	}
	return proto.RunRequest{
		Argv:  []string{name, "-test.run=TestHelperProcess", "--", mode},
		Env:   map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
		Files: []proto.InputFile{{Path: name, Data: data, SHA256: sha(data)}},
	}, data
}

func TestShippedProgramLinkedFromCache(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	req, data := shippedHelper(t, "sameas")
	req.Env["SAME_AS"] = filepath.Join(h.cacheDir, sha(data))
	// The first run uploads the program; the second takes it from the cache.
	// Both must run the cached file itself, not a fresh copy, which Windows
	// Defender would scan again before letting it start.
	for _, label := range []string{"first run", "second run"} {
		res, out, errOut, err := h.run(t, context.Background(), req)
		if err != nil || res.ExitCode != 0 || out != "true" {
			t.Errorf("%s: res=%+v err=%v stdout=%q stderr=%q", label, res, err, out, errOut)
		}
	}
}

func TestChangedCachedProgramIsResent(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	req, data := shippedHelper(t, "echo")
	check := func(label string) {
		t.Helper()
		res, out, errOut, err := h.run(t, context.Background(), req)
		if err != nil || res.ExitCode != 3 || out != "hello from job\n" {
			t.Fatalf("%s: res=%+v err=%v stdout=%q stderr=%q", label, res, err, out, errOut)
		}
	}
	check("first run")
	// Jobs run a link to the cache entry, so a changed entry must be caught
	// before the next job runs it.
	entry := filepath.Join(h.cacheDir, sha(data))
	if err := os.WriteFile(entry, []byte("not the program"), 0o700); err != nil {
		t.Fatal(err)
	}
	check("run after the cached program changed")
	if got, _ := os.ReadFile(entry); !bytes.Equal(got, data) {
		t.Errorf("cache entry was not restored (%d bytes, want %d)", len(got), len(data))
	}
}

func TestFileCache(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	content := []byte("cached input")
	hash := sha(content)
	req := helperRequest(t, "files", nil)
	req.Files = []proto.InputFile{{Path: "data/in.txt", Data: content, SHA256: hash}}
	req.Collect = []string{"out/result.txt"}

	run := func(label string) string {
		t.Helper()
		res, _, errOut, err := h.run(t, context.Background(), req)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("%s: res=%+v err=%v stderr=%q", label, res, err, errOut)
		}
		got, _ := os.ReadFile(filepath.Join(h.outDir, "out", "result.txt"))
		return string(got)
	}
	check := func(label string) {
		t.Helper()
		if got := run(label); got != "CACHED INPUT" {
			t.Fatalf("%s: result = %q", label, got)
		}
	}

	check("first run sends the data")
	entry := filepath.Join(h.cacheDir, hash)
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("file not cached: %v", err)
	}
	// Prove the second run reads the agent's copy rather than resent data:
	// mark the cached copy and see the mark in the job's output.
	if err := os.WriteFile(entry, []byte("from cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := run("second run"); got != "FROM CACHE" {
		t.Fatalf("second run did not use the cache: result = %q", got)
	}
	if err := os.WriteFile(entry, content, 0o600); err != nil {
		t.Fatal(err)
	}
	check("second run uses the cache")

	// The agent loses its copy; the client must notice and resend.
	if err := os.Remove(filepath.Join(h.cacheDir, hash)); err != nil {
		t.Fatal(err)
	}
	check("run after cache loss")
}

func TestNewClientUsesAgentCache(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	content := []byte("cached input")
	hash := sha(content)
	req := helperRequest(t, "files", nil)
	req.Files = []proto.InputFile{{Path: "data/in.txt", Data: content, SHA256: hash}}
	req.Collect = []string{"out/result.txt"}
	if res, _, errOut, err := h.run(t, context.Background(), req); err != nil || res.ExitCode != 0 {
		t.Fatalf("first run: res=%+v err=%v stderr=%q", res, err, errOut)
	}

	// A new client (a new pool command) must ask for the agent's copy rather
	// than upload the file again. Mark the cached copy to see which one ran.
	if err := os.WriteFile(filepath.Join(h.cacheDir, hash), []byte("from cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := client.New()
	fresh.Port = h.client.Port
	var out, errb bytes.Buffer
	res, err := fresh.Run(context.Background(), h.host, req, client.Output{Stdout: &out, Stderr: &errb, Dir: h.outDir})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("new client: res=%+v err=%v stderr=%q", res, err, errb.String())
	}
	if got, _ := os.ReadFile(filepath.Join(h.outDir, "out", "result.txt")); string(got) != "FROM CACHE" {
		t.Errorf("new client uploaded the file again: result = %q", got)
	}
}

func TestFileCacheRejectsWrongHash(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	req := helperRequest(t, "pwd", nil)
	req.Files = []proto.InputFile{{Path: "x.bin", Data: []byte("real"), SHA256: sha([]byte("forged"))}}
	res, _, _, err := h.run(t, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 127 || !strings.Contains(res.RemoteError, "does not match") {
		t.Errorf("result = %+v", res)
	}
	if entries, _ := os.ReadDir(h.cacheDir); len(entries) != 0 {
		t.Errorf("cache holds %d entries after a forged upload", len(entries))
	}
}

func TestUnsafePathsRejected(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	for _, p := range []string{"../escape.txt", "/etc/passwd", `C:\Windows\x`, "a/../../b"} {
		req := helperRequest(t, "pwd", nil)
		req.Files = []proto.InputFile{{Path: p, Data: []byte("x")}}
		res, _, _, err := h.run(t, context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != 127 || !strings.Contains(res.RemoteError, "unsafe path") {
			t.Errorf("input %q: result = %+v", p, res)
		}
	}
	req := helperRequest(t, "pwd", nil)
	req.Collect = []string{"../*"}
	res, _, _, err := h.run(t, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.RemoteError, "unsafe path") {
		t.Errorf("collect ../*: result = %+v", res)
	}
}

func TestBrowserRequestsRejected(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	base := "http://" + net.JoinHostPort(h.host, strconv.Itoa(h.client.Port))
	body := `{"argv":["whoami"]}`
	send := func(method, path, contentType string, headers map[string]string) int {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// A web page can send text/plain without a preflight; it must be refused.
	if code := send("POST", "/v1/run", "text/plain", nil); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain run: %d, want 415", code)
	}
	if code := send("POST", "/v1/run", "application/json", map[string]string{"Origin": "http://evil.example"}); code != http.StatusForbidden {
		t.Errorf("run with Origin: %d, want 403", code)
	}
	if code := send("GET", "/v1/jobs", "", map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
		t.Errorf("jobs with Sec-Fetch-Site cross-site: %d, want 403", code)
	}
	if code := send("GET", "/v1/info", "", map[string]string{"Sec-Fetch-Site": "none"}); code != http.StatusOK {
		t.Errorf("info typed into the address bar (Sec-Fetch-Site none): %d, want 200", code)
	}

	// With allowed hosts set, a rebound hostname is refused.
	h.agent.SetAllowedHosts([]string{net.JoinHostPort(h.host, strconv.Itoa(h.client.Port))})
	if code := send("GET", "/v1/info", "", nil); code != http.StatusOK {
		t.Errorf("info with the agent's own Host: %d, want 200", code)
	}
	req, _ := http.NewRequest("GET", base+"/v1/info", nil)
	req.Host = "attacker.example:7470"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("info with a rebound Host: %d, want 403", resp.StatusCode)
	}
	// The real client still works.
	if res, _, _, err := h.run(t, context.Background(), helperRequest(t, "echo", nil)); err != nil || res.ExitCode != 3 {
		t.Errorf("client run: %+v %v", res, err)
	}
}

func TestTooManyFilesRejected(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	req := helperRequest(t, "pwd", nil)
	for i := 0; i <= 10000; i++ {
		req.Files = append(req.Files, proto.InputFile{Path: fmt.Sprintf("f%d", i)})
	}
	_, _, _, err := h.run(t, context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "more than 10000 files") {
		t.Errorf("err = %v", err)
	}
}

func TestStaleJobDirsRemovedAtStartup(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "job-123")
	if err := os.MkdirAll(filepath.Join(stale, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "not-a-job")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.New(agent.Config{NodeName: "n", MaxJobs: 1, WorkDir: dir, Auth: allowAll{}, Logger: log.New(io.Discard, "", 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale job folder survived startup")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("an unrelated file was removed")
	}
}

func TestJobHistory(t *testing.T) {
	h := newHarness(t, 2, allowAll{})
	if _, _, _, err := h.run(t, context.Background(), helperRequest(t, "echo", nil)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _, _, _ = h.run(t, ctx, helperRequest(t, "sleep", nil)) }()
	waitFor(t, "sleep job to start", func() bool { return h.agent.Running() == 1 })
	if n := len(h.agent.Jobs().Running); n != 1 {
		t.Fatalf("Running() is 1 but Jobs() lists %d running jobs", n)
	}

	jobs, err := h.client.Jobs(context.Background(), h.host)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Running) != 1 || !jobs.Running[0].Running || !strings.Contains(jobs.Running[0].Command, "sleep") {
		t.Errorf("running = %+v", jobs.Running)
	}
	if len(jobs.Recent) != 1 {
		t.Fatalf("recent = %+v", jobs.Recent)
	}
	r := jobs.Recent[0]
	if r.ExitCode != 3 || r.From != "tester" || r.Running || !strings.Contains(r.Command, "echo") || r.Started.IsZero() {
		t.Errorf("recent[0] = %+v", r)
	}

	cancel()
	waitFor(t, "sleep job to stop", func() bool { return h.agent.Running() == 0 })
	jobs = h.agent.Jobs()
	if len(jobs.Running) != 0 || len(jobs.Recent) != 2 || jobs.Recent[0].ExitCode != 130 {
		t.Errorf("after cancel: %+v", jobs)
	}
}

// fixedProbe reports a constant snapshot that a test can change.
type fixedProbe struct{ snap sysinfo.Snapshot }

func (p *fixedProbe) get() sysinfo.Snapshot { return p.snap }

func TestBatteryPolicy(t *testing.T) {
	probe := &fixedProbe{sysinfo.Snapshot{CPUBusyPct: 10, Power: sysinfo.Power{Known: true, HasBattery: true, OnBattery: true, BatteryPct: 55}}}
	h := newHarnessWith(t, 2, allowAll{}, func(c *agent.Config) { c.Probe = probe.get; c.BusyThreshold = 80 })

	info, err := h.client.Info(context.Background(), h.host)
	if err != nil {
		t.Fatal(err)
	}
	if info.Accepting || info.Reason != "on battery" || !info.OnBattery || info.BatteryPct != 55 {
		t.Errorf("info on battery = %+v", info)
	}
	_, _, _, err = h.run(t, context.Background(), helperRequest(t, "echo", nil))
	if !errors.Is(err, client.ErrBusy) || !strings.Contains(err.Error(), "on battery") {
		t.Errorf("run on battery: err = %v, want ErrBusy mentioning battery", err)
	}

	probe.snap.Power.OnBattery = false // plugged back in
	if res, _, _, err := h.run(t, context.Background(), helperRequest(t, "echo", nil)); err != nil || res.ExitCode != 3 {
		t.Errorf("run on AC: res=%+v err=%v", res, err)
	}
}

func TestLendOnBattery(t *testing.T) {
	probe := &fixedProbe{sysinfo.Snapshot{CPUBusyPct: -1, Power: sysinfo.Power{Known: true, HasBattery: true, OnBattery: true}}}
	h := newHarnessWith(t, 1, allowAll{}, func(c *agent.Config) { c.Probe = probe.get; c.LendOnBattery = true })
	if res, _, _, err := h.run(t, context.Background(), helperRequest(t, "echo", nil)); err != nil || res.ExitCode != 3 {
		t.Errorf("res=%+v err=%v", res, err)
	}
}

func TestBusyThreshold(t *testing.T) {
	probe := &fixedProbe{sysinfo.Snapshot{CPUBusyPct: 92, Power: sysinfo.Power{BatteryPct: -1}}}
	h := newHarnessWith(t, 4, allowAll{}, func(c *agent.Config) { c.Probe = probe.get; c.BusyThreshold = 80 })

	info, err := h.client.Info(context.Background(), h.host)
	if err != nil {
		t.Fatal(err)
	}
	if info.Accepting || !strings.Contains(info.Reason, "cpu busy (92%)") {
		t.Errorf("info = %+v", info)
	}
	if want := float64(info.LogicalCPUs) * 0.08; info.IdleCPUs < want-0.01 || info.IdleCPUs > want+0.01 {
		t.Errorf("IdleCPUs = %v, want %v", info.IdleCPUs, want)
	}
	if _, _, _, err := h.run(t, context.Background(), helperRequest(t, "echo", nil)); !errors.Is(err, client.ErrBusy) {
		t.Errorf("run while busy: err = %v, want ErrBusy", err)
	}

	probe.snap.CPUBusyPct = 30
	info, _ = h.client.Info(context.Background(), h.host)
	if !info.Accepting || info.Reason != "" {
		t.Errorf("info after load dropped = %+v", info)
	}

	// Load from the agent's own jobs doesn't count: 92% busy, 75% of it
	// ours, leaves 17% from other programs.
	probe.snap.CPUBusyPct, probe.snap.OwnBusyPct = 92, 75
	info, _ = h.client.Info(context.Background(), h.host)
	if !info.Accepting {
		t.Errorf("refused because of its own jobs: %+v", info)
	}
	// But other programs past the threshold still pause it.
	probe.snap.CPUBusyPct, probe.snap.OwnBusyPct = 100, 15
	info, _ = h.client.Info(context.Background(), h.host)
	if info.Accepting || !strings.Contains(info.Reason, "cpu busy (85%)") {
		t.Errorf("not paused by other programs: %+v", info)
	}
}

func TestInfoReportsSpecsAndCapacity(t *testing.T) {
	specs := hwinfo.Specs{CPUModel: "Example CPU", LogicalCPUs: 16, PhysicalCores: 8, MemoryMB: 16384}
	measured := proto.Capacity{Kernel: "test", Levels: []proto.Level{{Jobs: 1, Rate: 90}, {Jobs: 8, Rate: 600}}}
	h := newHarnessWith(t, 1, allowAll{}, func(c *agent.Config) { c.Specs, c.Capacity = &specs, &measured })
	info, err := h.client.Info(context.Background(), h.host)
	if err != nil {
		t.Fatal(err)
	}
	if info.Specs == nil || *info.Specs != specs {
		t.Errorf("specs = %+v", info.Specs)
	}
	if info.Capacity == nil || info.Capacity.RateAt(8) != 600 {
		t.Errorf("capacity = %+v", info.Capacity)
	}
	// An agent configured without them leaves them out.
	plain := newHarness(t, 1, allowAll{})
	if info, err := plain.client.Info(context.Background(), plain.host); err != nil || info.Specs != nil || info.Capacity != nil {
		t.Errorf("unconfigured agent: specs %v, capacity %v, err %v", info.Specs, info.Capacity, err)
	}
}

func TestInfoReportsLiveLimit(t *testing.T) {
	// 2 GB free of 16 GB keeps 1.6 GB for the owner: no room for a 512 MB job.
	probe := &fixedProbe{sysinfo.Snapshot{CPUBusyPct: -1, OwnBusyPct: -1, Power: sysinfo.Power{BatteryPct: -1},
		Memory: sysinfo.Memory{Known: true, TotalMB: 16384, FreeMB: 2048}}}
	h := newHarnessWith(t, 4, allowAll{}, func(c *agent.Config) { c.Probe = probe.get })
	info, err := h.client.Info(context.Background(), h.host)
	if err != nil {
		t.Fatal(err)
	}
	if info.Accepting || info.JobLimit != 0 || info.FreeMemoryMB != 2048 || info.JobMemoryMB != 512 || info.JobMemoryMeasured ||
		!strings.HasPrefix(info.Reason, "taking no jobs for now: 2.0 GB of memory free") || !strings.Contains(info.LimitReason, "memory") {
		t.Errorf("short of memory: %+v", info)
	}
	if _, _, _, err := h.run(t, context.Background(), helperRequest(t, "echo", nil)); !errors.Is(err, client.ErrBusy) {
		t.Errorf("run while short of memory: err = %v, want ErrBusy", err)
	}

	probe.snap.Memory.FreeMB = 8192 // the owner closed something
	info, _ = h.client.Info(context.Background(), h.host)
	if !info.Accepting || info.JobLimit != 4 || info.LimitReason != "" {
		t.Errorf("with memory free: %+v", info)
	}
}

func TestJobMemoryIsMeasured(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	if res, _, stderr, err := h.run(t, context.Background(), helperRequest(t, "alloc", nil)); err != nil || res.ExitCode != 0 {
		t.Fatalf("alloc job: res=%+v err=%v stderr=%s", res, err, stderr)
	}
	info, err := h.client.Info(context.Background(), h.host)
	if err != nil {
		t.Fatal(err)
	}
	// The job used 200 MB on top of the test binary itself.
	if !info.JobMemoryMeasured || info.JobMemoryMB < 200 || info.JobMemoryMB > 1024 {
		t.Errorf("job memory = %d MB (measured %v), want 200-1024", info.JobMemoryMB, info.JobMemoryMeasured)
	}
}

func TestInfoWithoutProbeIsAcceptingAndUnknown(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	info, err := h.client.Info(context.Background(), h.host)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Accepting || info.CPUBusyPct != -1 || info.IdleCPUs != float64(info.LogicalCPUs) || info.BatteryPct != -1 {
		t.Errorf("info = %+v", info)
	}
}

func TestEmptyCommandRejected(t *testing.T) {
	h := newHarness(t, 1, allowAll{})
	_, _, _, err := h.run(t, context.Background(), proto.RunRequest{})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v, want a 400 error", err)
	}
}
