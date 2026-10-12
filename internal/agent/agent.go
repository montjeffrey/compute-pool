// Package agent runs jobs submitted by other devices on the tailnet.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/sysinfo"
)

// Config configures an Agent.
type Config struct {
	NodeName string     // this device's short tailnet name
	MaxJobs  int        // concurrent jobs; further requests get 503
	WorkDir  string     // parent of each job's scratch directory
	Auth     Authorizer // required
	Logger   *log.Logger

	// Probe reports current CPU load and power state. If nil, both are
	// treated as unknown and never cause a refusal.
	Probe func() sysinfo.Snapshot
	// LendOnBattery lets a laptop accept jobs while unplugged.
	LendOnBattery bool
	// BusyThreshold refuses new jobs while system CPU use is at or above
	// this percentage, whether the load comes from the owner or from jobs
	// already running. Zero disables the check.
	BusyThreshold float64

	// CacheDir keeps shipped files by content hash between jobs; empty
	// disables caching. CacheBytes caps its size (default 2 GiB).
	CacheDir   string
	CacheBytes int64
}

// Agent serves the pool HTTP API.
type Agent struct {
	cfg     Config
	cache   *fileCache // nil when caching is off
	slots   chan struct{}
	running atomic.Int32
	seq     atomic.Uint64
	started time.Time

	histMu  sync.Mutex
	active  map[string]*proto.JobRecord
	history []proto.JobRecord // finished jobs, oldest first, at most historySize

	hostsMu      sync.RWMutex
	allowedHosts map[string]bool // empty: any Host header
}

const historySize = 50

func describeCommand(req proto.RunRequest) string {
	s := strings.Join(req.Argv, " ")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

func (a *Agent) jobStarted(id, from string, req proto.RunRequest) {
	a.histMu.Lock()
	defer a.histMu.Unlock()
	a.active[id] = &proto.JobRecord{ID: id, From: from, Command: describeCommand(req), Started: time.Now(), Running: true}
}

func (a *Agent) jobFinished(id string, code int, dur time.Duration, err error) {
	a.histMu.Lock()
	defer a.histMu.Unlock()
	rec, ok := a.active[id]
	if !ok {
		return
	}
	delete(a.active, id)
	rec.Running, rec.ExitCode, rec.DurationMS = false, code, dur.Milliseconds()
	if err != nil {
		rec.Error = err.Error()
	}
	a.history = append(a.history, *rec)
	if len(a.history) > historySize {
		a.history = a.history[len(a.history)-historySize:]
	}
}

// Jobs reports running and recent jobs, newest first.
func (a *Agent) Jobs() proto.Jobs {
	a.histMu.Lock()
	defer a.histMu.Unlock()
	out := proto.Jobs{Running: []proto.JobRecord{}, Recent: make([]proto.JobRecord, 0, len(a.history))}
	for _, r := range a.active {
		out.Running = append(out.Running, *r)
	}
	sort.Slice(out.Running, func(i, j int) bool { return out.Running[i].Started.After(out.Running[j].Started) })
	for i := len(a.history) - 1; i >= 0; i-- {
		out.Recent = append(out.Recent, a.history[i])
	}
	return out
}

func (a *Agent) handleJobs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a.Jobs())
}

// New validates cfg and prepares the work directory.
func New(cfg Config) (*Agent, error) {
	if cfg.Auth == nil {
		return nil, errors.New("agent: Auth is required")
	}
	if cfg.MaxJobs < 1 {
		return nil, fmt.Errorf("agent: MaxJobs must be at least 1, got %d", cfg.MaxJobs)
	}
	if cfg.WorkDir == "" {
		return nil, errors.New("agent: WorkDir is required")
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: create work dir: %w", err)
	}
	// No job is running yet, so any job folder here was left by an agent
	// that crashed or was killed.
	if stale, err := filepath.Glob(filepath.Join(cfg.WorkDir, "job-*")); err == nil {
		for _, p := range stale {
			os.RemoveAll(p)
		}
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	a := &Agent{
		cfg:     cfg,
		slots:   make(chan struct{}, cfg.MaxJobs),
		started: time.Now(),
		active:  make(map[string]*proto.JobRecord),
	}
	if cfg.CacheDir != "" {
		if cfg.CacheBytes <= 0 {
			cfg.CacheBytes = 2 << 30
		}
		c, err := newFileCache(cfg.CacheDir, cfg.CacheBytes)
		if err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		a.cache = c
	}
	return a, nil
}

// missingFiles lists hashes a request wants from the cache that it lacks. The
// job's program is linked rather than copied, so its entry is also checked
// against its hash, and one that has changed counts as missing.
func (a *Agent) missingFiles(req proto.RunRequest) []string {
	program := shippedProgram(req)
	var missing []string
	for _, f := range req.Files {
		if !f.Cached {
			continue
		}
		ok := a.cache != nil && a.cache.has(f.SHA256)
		if ok && program != "" {
			if rel, err := proto.CleanRelPath(f.Path); err == nil && rel == program {
				ok = a.cache.verify(f.SHA256)
			}
		}
		if !ok {
			missing = append(missing, f.SHA256)
		}
	}
	return missing
}

// Running reports how many jobs are executing.
func (a *Agent) Running() int { return int(a.running.Load()) }

// Handler returns the authenticated HTTP API.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", a.handleInfo)
	mux.HandleFunc("POST /v1/run", a.handleRun)
	mux.HandleFunc("GET /v1/jobs", a.handleJobs)
	return a.authenticate(mux)
}

type identityKey struct{}

// SetAllowedHosts limits which Host headers the agent answers ("ip:port",
// "name:port"). Empty means any. Checking it stops DNS rebinding, where a web
// page points a hostname it controls at the agent's address.
func (a *Agent) SetAllowedHosts(hosts []string) {
	set := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		set[strings.ToLower(h)] = true
	}
	a.hostsMu.Lock()
	a.allowedHosts = set
	a.hostsMu.Unlock()
}

func (a *Agent) hostAllowed(host string) bool {
	a.hostsMu.RLock()
	defer a.hostsMu.RUnlock()
	return len(a.allowedHosts) == 0 || a.allowedHosts[strings.ToLower(host)]
}

// fromBrowser reports whether a request was sent by a web page. The pool
// client never sends these headers; browsers always do for cross-site
// requests. Without this check, any page open in a browser on an allowed
// device could post jobs, because the request comes from that device's IP.
func fromBrowser(r *http.Request) bool {
	if r.Header.Get("Origin") != "" {
		return true
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site != "" && site != "none"
}

func (a *Agent) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !a.hostAllowed(r.Host) || fromBrowser(r) {
			a.cfg.Logger.Printf("refused %s %s from %s: browser-originated request or unexpected Host %q", r.Method, r.URL.Path, host, r.Host)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		id, err := a.cfg.Auth.Authorize(r.Context(), host)
		if err != nil {
			// Details stay in the local log; the caller only learns it was refused.
			a.cfg.Logger.Printf("refused %s %s from %s: %v", r.Method, r.URL.Path, host, err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}

func (a *Agent) snapshot() sysinfo.Snapshot {
	if a.cfg.Probe == nil {
		return sysinfo.Snapshot{CPUBusyPct: -1, Power: sysinfo.Power{BatteryPct: -1}}
	}
	return a.cfg.Probe()
}

// refusal returns why a new job would be refused right now, or "" if it
// would be accepted. Slot availability is checked separately, atomically,
// when a job actually arrives.
func (a *Agent) refusal(s sysinfo.Snapshot) string {
	if s.Power.OnBattery && !a.cfg.LendOnBattery {
		return "on battery"
	}
	if a.cfg.BusyThreshold > 0 && s.CPUBusyPct >= a.cfg.BusyThreshold {
		return fmt.Sprintf("cpu busy (%.0f%%)", s.CPUBusyPct)
	}
	return ""
}

func (a *Agent) info() proto.Info {
	s := a.snapshot()
	cpus := runtime.NumCPU()
	idle := float64(cpus)
	if s.CPUBusyPct >= 0 {
		idle = float64(cpus) * (100 - s.CPUBusyPct) / 100
	}
	info := proto.Info{
		Node:        a.cfg.NodeName,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		LogicalCPUs: cpus,
		MaxJobs:     a.cfg.MaxJobs,
		Running:     a.Running(),
		Version:     proto.Version,
		StartedAt:   a.started,
		CPUBusyPct:  s.CPUBusyPct,
		IdleCPUs:    idle,
		HasBattery:  s.Power.HasBattery,
		OnBattery:   s.Power.OnBattery,
		BatteryPct:  s.Power.BatteryPct,
	}
	info.Reason = a.refusal(s)
	if info.Reason == "" && info.Running >= info.MaxJobs {
		info.Reason = fmt.Sprintf("all %d job slots in use", info.MaxJobs)
	}
	info.Accepting = info.Reason == ""
	return info
}

func (a *Agent) handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a.info())
}

// maxFilesPerRequest bounds how many input files one job may ship.
const maxFilesPerRequest = 10000

func (a *Agent) handleRun(w http.ResponseWriter, r *http.Request) {
	// A JSON content type cannot be sent cross-site without a CORS preflight,
	// which the agent never answers: a second guard against web pages.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	// Refuse before reading the body, so busy agents don't buffer uploads
	// they will reject, and at most MaxJobs bodies are in memory at once.
	if reason := a.refusal(a.snapshot()); reason != "" {
		http.Error(w, reason, http.StatusServiceUnavailable)
		return
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		http.Error(w, fmt.Sprintf("all %d job slots in use", a.cfg.MaxJobs), http.StatusServiceUnavailable)
		return
	}

	var req proto.RunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, proto.MaxRequestBytes)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		http.Error(w, "bad request: argv is empty", http.StatusBadRequest)
		return
	}
	if len(req.Files) > maxFilesPerRequest {
		http.Error(w, fmt.Sprintf("bad request: more than %d files", maxFilesPerRequest), http.StatusBadRequest)
		return
	}
	if missing := a.missingFiles(req); len(missing) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_ = json.NewEncoder(w).Encode(proto.MissingFiles{Missing: missing})
		return
	}
	id, _ := r.Context().Value(identityKey{}).(Identity)
	jobID := fmt.Sprintf("%s-%s-%d", a.cfg.NodeName, time.Now().Format("20060102T150405"), a.seq.Add(1))
	a.cfg.Logger.Printf("job %s from %s: %q (shell=%v)", jobID, id.Node, req.Argv, req.Shell)

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := w.(http.Flusher)
	fw := proto.NewFrameWriter(w, func() {
		if flusher != nil {
			flusher.Flush()
		}
	})
	_ = fw.Write(proto.Frame{Type: proto.FrameStart, JobID: jobID, Node: a.cfg.NodeName})

	// Register the job before counting it, and record it before uncounting
	// it, so Running() and Jobs() always agree.
	a.jobStarted(jobID, id.Node, req)
	a.running.Add(1)
	code, dur, err := a.execute(r.Context(), jobID, req, fw)
	a.jobFinished(jobID, code, dur, err)
	a.running.Add(-1)
	if err != nil {
		_ = fw.Write(proto.Frame{Type: proto.FrameError, Message: err.Error()})
	}
	_ = fw.Write(proto.Frame{Type: proto.FrameExit, ExitCode: code, DurationMS: dur.Milliseconds()})
	a.cfg.Logger.Printf("job %s done: exit=%d in %s", jobID, code, dur.Round(time.Millisecond))
}
