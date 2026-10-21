package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/montjeffrey/compute-pool/internal/agent"
	"github.com/montjeffrey/compute-pool/internal/capacity"
	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/sysinfo"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

// agentFlags are shared by `pool agent` and `pool install`, which validates
// them before writing them into the startup task.
type agentFlags struct {
	allow         *string
	port          *int
	maxJobs       *int
	workDir       *string
	listen        *string
	lendOnBattery *bool
	busyThreshold *float64
	cacheMB       *int64
	jobMemory     *int64
	logFile       *string
	hideConsole   *bool
}

func newAgentFlags(name string, stderr io.Writer) (*flag.FlagSet, *agentFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs, &agentFlags{
		allow:         fs.String("allow", "", "comma-separated tailnet device names allowed to submit jobs (this device is always allowed)"),
		port:          fs.Int("port", proto.DefaultPort, "TCP port to listen on"),
		maxJobs:       fs.Int("max-jobs", max(1, runtime.NumCPU()/2), "jobs to run at once"),
		workDir:       fs.String("work-dir", defaultWorkDir(), "directory for per-job scratch folders"),
		listen:        fs.String("listen", "", "listen address (default: this device's Tailscale IPv4 and --port)"),
		lendOnBattery: fs.Bool("lend-on-battery", false, "accept jobs while this device runs on battery"),
		busyThreshold: fs.Float64("busy-threshold", 80, "refuse new jobs while CPU use is at or above this percent (0 = never)"),
		cacheMB:       fs.Int64("cache-mb", 2048, "disk space for caching shipped files between jobs (0 = no cache)"),
		jobMemory:     fs.Int64("job-memory", agent.DefaultJobMemoryMB, "memory in MB to assume each job needs until jobs have been measured"),
		logFile:       fs.String("log-file", "", "append the agent's log to this file instead of stderr"),
		hideConsole:   fs.Bool("hide-console", false, "detach from the console window (used by the startup task)"),
	}
}

func cmdAgent(ctx context.Context, args []string, stderr io.Writer) error {
	fs, f := newAgentFlags("agent", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *f.hideConsole {
		hideConsole()
	}
	logOut, logPath := stderr, ""
	if *f.logFile != "" {
		abs, err := filepath.Abs(*f.logFile)
		if err != nil {
			return err
		}
		logPath = abs
		lf, err := newRotatingLog(logPath)
		if err != nil {
			return err
		}
		defer lf.Close()
		logOut = lf
	}
	// Microseconds let pool report tell which jobs overlapped.
	logger := log.New(logOut, "", log.LstdFlags|log.Lmicroseconds)
	if err := containChildren(); err != nil {
		logger.Printf("warning: jobs may outlive the agent if it is killed: %v", err)
	}

	cacheDir := ""
	if *f.cacheMB > 0 {
		cacheDir = filepath.Join(filepath.Dir(filepath.Clean(*f.workDir)), "cache")
	}
	sampler := sysinfo.NewSampler()
	// Telling the agent's own jobs apart from the owner's work keeps the
	// busy threshold from pausing the agent because of its own jobs.
	if read := ownCPUReader(); read != nil {
		sampler.SetOwnReader(read)
	} else {
		logger.Printf("warning: can't tell this agent's jobs from other programs; the busy threshold counts both")
	}
	go sampler.Run(ctx, 2*time.Second)

	// Other devices use these to weigh this one; pool setup saves the
	// measurement, and an agent without one simply doesn't report it.
	specs := hwinfo.Detect()
	measured, err := capacity.Load(capacity.DefaultPath())
	if err != nil {
		measured = nil
		if !errors.Is(err, os.ErrNotExist) {
			logger.Printf("not reporting measured speed: %v", err)
		}
	}

	var a *agent.Agent
	for {
		// At boot or after waking, Tailscale may not be connected yet; wait
		// for it instead of exiting.
		st, err := waitForTailscale(ctx, logger)
		if err != nil {
			logger.Printf("agent stopped")
			return nil
		}
		if st.SelfTagged {
			// On a tagged device the "owner" is the shared pseudo-user for all
			// tagged devices, so the same-owner check would admit every one.
			return errors.New("refusing to run on a tagged Tailscale device: the agent must run on a device owned by a person")
		}
		if a == nil {
			allowed := append([]string{st.Self.Name}, strings.Split(*f.allow, ",")...)
			auth := agent.NewTailnetAuthorizer(st.SelfLogin, allowed)
			a, err = agent.New(agent.Config{
				NodeName:      st.Self.Name,
				MaxJobs:       *f.maxJobs,
				WorkDir:       *f.workDir,
				Auth:          auth,
				Logger:        logger,
				Probe:         sampler.Snapshot,
				LendOnBattery: *f.lendOnBattery,
				BusyThreshold: *f.busyThreshold,
				CacheDir:      cacheDir,
				CacheBytes:    *f.cacheMB << 20,
				JobMemoryMB:   *f.jobMemory,
				Specs:         &specs,
				Capacity:      measured,
				LogPath:       logPath,
			})
			if err != nil {
				return err
			}
			pw := sampler.Snapshot().Power
			logger.Printf("pool agent %s on %s; owner %s; allowed devices: %s; max jobs %d",
				proto.Version, st.Self.Name, st.SelfLogin, strings.Join(auth.Allowed(), ", "), *f.maxJobs)
			logger.Printf("policy: lend on battery=%v; refuse new jobs at >= %.0f%% CPU; power now: %s",
				*f.lendOnBattery, *f.busyThreshold, describePower(pw.HasBattery, pw.OnBattery, pw.BatteryPct))
			if m := sampler.Snapshot().Memory; m.Known {
				logger.Printf("memory: %.1f of %.1f GB free; planning %d MB per job until jobs are measured; fewer jobs while other programs use the CPU or memory runs short",
					float64(m.FreeMB)/1024, float64(m.TotalMB)/1024, *f.jobMemory)
			}
		}

		addr, boundIP := *f.listen, ""
		if addr == "" {
			// Binding to the Tailscale address keeps the agent off LAN and
			// public interfaces entirely; the authorizer is a second line of
			// defense.
			boundIP = st.Self.IPv4
			addr = net.JoinHostPort(boundIP, strconv.Itoa(*f.port))
		} else {
			logger.Printf("warning: --listen %s may expose the agent beyond Tailscale; only the identity check protects it", addr)
		}
		a.SetAllowedHosts(agentHosts(addr, st.Self))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			logger.Printf("listen on %s: %v; retrying in 10s", addr, err)
			if !sleepCtx(ctx, 10*time.Second) {
				logger.Printf("agent stopped")
				return nil
			}
			continue
		}
		logger.Printf("listening at %s", ln.Addr())
		err = serveUntilBroken(ctx, a.Handler(), ln, boundIP, logger)
		if ctx.Err() != nil {
			logger.Printf("agent stopped")
			return nil
		}
		logger.Printf("listener stopped (%v); binding again", err)
		sleepCtx(ctx, 2*time.Second)
	}
}

// agentHosts lists the Host headers clients legitimately send: the listen
// address and this device's Tailscale IP and names on the same port.
func agentHosts(addr string, self tsutil.Peer) []string {
	hosts := []string{addr}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return hosts
	}
	for _, h := range []string{self.IPv4, self.DNSName, self.Name} {
		if h != "" {
			hosts = append(hosts, net.JoinHostPort(h, port))
		}
	}
	return hosts
}

func waitForTailscale(ctx context.Context, logger *log.Logger) (tsutil.Status, error) {
	for i := 0; ; i++ {
		st, err := tsutil.GetStatus(ctx)
		if err == nil && st.SelfLogin == "" {
			err = errors.New("could not determine which Tailscale user owns this device")
		}
		if err == nil {
			if i > 0 {
				logger.Printf("Tailscale is up")
			}
			return st, nil
		}
		if i%12 == 0 { // about once a minute
			logger.Printf("waiting for Tailscale: %v", err)
		}
		if !sleepCtx(ctx, 5*time.Second) {
			return tsutil.Status{}, ctx.Err()
		}
	}
}

// serveUntilBroken serves on ln until ctx ends or two health checks in a row
// fail. Sleep, network changes and Tailscale restarts can leave a listener
// bound to an address that no longer works without any error surfacing.
func serveUntilBroken(ctx context.Context, h http.Handler, ln net.Listener, boundIP string, logger *log.Logger) error {
	srv := newServer(h)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = srv.Shutdown(shutdownCtx)
			cancel()
			<-errc
			return ctx.Err()
		case err := <-errc:
			return err
		case <-tick.C:
			reason := checkListener(ctx, ln.Addr().String(), boundIP)
			if reason == "" {
				failures = 0
				continue
			}
			if failures++; failures < 2 {
				continue
			}
			logger.Printf("health check failed: %s", reason)
			_ = srv.Close()
			<-errc
			return errors.New(reason)
		}
	}
}

func checkListener(ctx context.Context, addr, boundIP string) string {
	if boundIP != "" {
		st, err := tsutil.GetStatus(ctx)
		if err != nil {
			return "tailscale: " + err.Error()
		}
		if st.Self.IPv4 != boundIP {
			return fmt.Sprintf("Tailscale address changed from %s to %s", boundIP, st.Self.IPv4)
		}
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return "cannot reach own listener: " + err.Error()
	}
	conn.Close()
	return ""
}

// newServer applies the limits shared by the agent and the dashboard. There
// is no WriteTimeout because job output streams for as long as a job runs.
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

const maxLogBytes = 10 << 20

// openLogFile opens path for appending, first moving it to path.1 if it has
// grown past maxLogBytes.
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogBytes {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// rotatingLog is a log file that moves itself to path.1 whenever it passes
// maxLogBytes, so a long-running agent's log stays bounded.
type rotatingLog struct {
	path string
	mu   sync.Mutex
	f    *os.File
	size int64
}

func newRotatingLog(path string) (*rotatingLog, error) {
	f, err := openLogFile(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &rotatingLog{path: path, f: f, size: info.Size()}, nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(p)) > maxLogBytes {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotate moves the log to path.1 and starts a new one. On Windows the rename
// fails while another program reads the log (pool report does); the log then
// keeps growing and rotation is tried again on the next write, rather than
// emptying the file in place.
func (l *rotatingLog) rotate() error {
	l.f.Close()
	_ = os.Remove(l.path + ".1")
	renamed := os.Rename(l.path, l.path+".1") == nil
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	l.f = f
	if renamed {
		l.size = 0
	}
	return nil
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// sleepCtx waits for d and reports false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// defaultAgentLog is the standard place for an agent's log: next to the
// saved measurement, which on Windows is where pool install logs too.
func defaultAgentLog() string {
	return filepath.Join(filepath.Dir(capacity.DefaultPath()), "agent.log")
}
