package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/montjeffrey/compute-pool/internal/agent"
	"github.com/montjeffrey/compute-pool/internal/client"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

type dashDevice struct {
	Name   string      `json:"name"`
	OS     string      `json:"os"`
	Self   bool        `json:"self"`
	Status string      `json:"status"` // ready, busy, paused, refused, no-agent
	Detail string      `json:"detail,omitempty"`
	Info   *proto.Info `json:"info,omitempty"`
}

type dashJob struct {
	Device string `json:"device"`
	proto.JobRecord
}

type dashState struct {
	Updated   time.Time    `json:"updated"`
	Ready     int          `json:"ready"`
	TotalCPUs int          `json:"total_cpus"`
	IdleCPUs  float64      `json:"idle_cpus"`
	Devices   []dashDevice `json:"devices"`
	Running   []dashJob    `json:"running"`
	Recent    []dashJob    `json:"recent"`
}

const dashRecentLimit = 40

// buildState turns device probes and their job lists into the dashboard's
// view: devices with an agent first, then the rest, each group by name.
func buildState(results []probeResult, jobs map[string]proto.Jobs, now time.Time) dashState {
	s := dashState{Updated: now, Devices: []dashDevice{}, Running: []dashJob{}, Recent: []dashJob{}}
	for _, r := range results {
		d := dashDevice{Name: r.peer.Name, OS: r.peer.OS, Self: r.peer.Self}
		switch {
		case r.err == nil:
			info := r.info
			d.Info = &info
			switch {
			case info.Accepting:
				d.Status = "ready"
			case info.MaxJobs > 0 && info.Running >= info.MaxJobs:
				d.Status, d.Detail = "busy", info.Reason // working at capacity, not a problem
			default:
				d.Status, d.Detail = "paused", info.Reason
			}
			s.TotalCPUs += info.LogicalCPUs
			if info.Accepting {
				s.Ready++
				s.IdleCPUs += info.IdleCPUs
			}
		case errors.Is(r.err, client.ErrForbidden):
			d.Status, d.Detail = "refused", "its agent does not allow this device"
		default:
			d.Status = "no-agent"
		}
		s.Devices = append(s.Devices, d)
	}
	sort.SliceStable(s.Devices, func(i, j int) bool {
		ai, aj := s.Devices[i].Info != nil, s.Devices[j].Info != nil
		if ai != aj {
			return ai
		}
		return s.Devices[i].Name < s.Devices[j].Name
	})
	for dev, js := range jobs {
		for _, j := range js.Running {
			s.Running = append(s.Running, dashJob{Device: dev, JobRecord: j})
		}
		for _, j := range js.Recent {
			s.Recent = append(s.Recent, dashJob{Device: dev, JobRecord: j})
		}
	}
	newest := func(js []dashJob) {
		sort.Slice(js, func(i, j int) bool { return js[i].Started.After(js[j].Started) })
	}
	newest(s.Running)
	newest(s.Recent)
	if len(s.Recent) > dashRecentLimit {
		s.Recent = s.Recent[:dashRecentLimit]
	}
	return s
}

type dashboard struct {
	gather func(context.Context) (dashState, error)
	ttl    time.Duration

	mu       sync.Mutex
	cached   dashState
	cachedAt time.Time
}

// state returns a recent snapshot so several open tabs don't each query
// every device.
func (d *dashboard) state(ctx context.Context) (dashState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.cachedAt.IsZero() && time.Since(d.cachedAt) < d.ttl {
		return d.cached, nil
	}
	s, err := d.gather(ctx)
	if err != nil {
		return dashState{}, err
	}
	d.cached, d.cachedAt = s, time.Now()
	return s, nil
}

func (d *dashboard) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, dashboardHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		s, err := d.state(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s)
	})
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// requireHost rejects requests whose Host header isn't one of hosts. Without
// it, a web page could use DNS rebinding to read the local dashboard.
func requireHost(hosts []string, next http.Handler) http.Handler {
	ok := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		ok[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok[strings.ToLower(r.Host)] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireDevice admits only tailnet devices the authorizer allows.
func requireDevice(auth agent.Authorizer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			_, err = auth.Authorize(r.Context(), host)
		}
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func cmdDashboard(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:7471", "address to serve the dashboard on")
	tailnet := fs.Bool("tailnet", false, "serve on this device's Tailscale address so your other devices can open it")
	allow := fs.String("allow", "", "with --tailnet, devices allowed to view it (this device always can)")
	open := fs.Bool("open", false, "open the dashboard in a browser")
	port := fs.Int("port", proto.DefaultPort, "agent TCP port")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c := client.New()
	c.Port = *port
	gather := func(ctx context.Context) (dashState, error) {
		st, err := tsutil.GetStatus(ctx)
		if err != nil {
			return dashState{}, err
		}
		results := probeAll(ctx, st, c, time.Second)
		var mu sync.Mutex
		var wg sync.WaitGroup
		jobs := map[string]proto.Jobs{}
		for _, r := range results {
			if r.err != nil {
				continue
			}
			wg.Add(1)
			go func(name, host string) {
				defer wg.Done()
				jctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				if js, err := c.Jobs(jctx, host); err == nil {
					mu.Lock()
					jobs[name] = js
					mu.Unlock()
				}
			}(r.peer.Name, r.peer.IPv4)
		}
		wg.Wait()
		return buildState(results, jobs, time.Now()), nil
	}
	d := &dashboard{gather: gather, ttl: time.Second}

	addr := *listen
	h := d.handler()
	var self tsutil.Peer
	if *tailnet {
		st, err := tsutil.GetStatus(ctx)
		if err != nil {
			return err
		}
		_, p, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("--listen: %w", err)
		}
		self = st.Self
		addr = net.JoinHostPort(st.Self.IPv4, p)
		allowed := append([]string{st.Self.Name}, strings.Split(*allow, ",")...)
		h = requireDevice(agent.NewTailnetAuthorizer(st.SelfLogin, allowed), h)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// Either way, only Host headers naming this server are answered, which
	// blocks DNS rebinding from a web page.
	if *tailnet {
		h = requireHost(agentHosts(ln.Addr().String(), self), h)
	} else {
		_, p, _ := net.SplitHostPort(ln.Addr().String())
		h = requireHost([]string{ln.Addr().String(), "localhost:" + p}, h)
	}
	url := "http://" + ln.Addr().String()
	fmt.Fprintf(stdout, "Dashboard: %s  (Ctrl+C to stop)\n", url)
	if *open {
		_ = openBrowser(url)
	}
	srv := newServer(h)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Compute pool</title>
<style>
:root {
  --bg: #f7f7f5; --panel: #ffffff; --text: #1d1d1b; --muted: #6b6b66; --line: #e4e4df;
  --ok: #1f7a4d; --ok-bg: #e3f3ea; --warn: #8a5a00; --warn-bg: #fbf0d9;
  --bad: #a3262a; --bad-bg: #f8e1e1; --idle: #6b6b66; --idle-bg: #ededea; --bar: #3d6fd8;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #151514; --panel: #1e1e1c; --text: #ececea; --muted: #9a9a94; --line: #2f2f2c;
    --ok: #6fd3a0; --ok-bg: #173826; --warn: #f0c46a; --warn-bg: #3a2e14;
    --bad: #f08a8d; --bad-bg: #3d1a1b; --idle: #9a9a94; --idle-bg: #2a2a27; --bar: #7aa2f7;
  }
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--text);
  font: 14px/1.45 system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 1100px; margin: 0 auto; padding: 24px 16px 48px; }
header { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
h1 { font-size: 20px; margin: 0; }
h2 { font-size: 15px; margin: 28px 0 10px; }
.muted { color: var(--muted); }
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); gap: 12px; margin-top: 18px; }
.tile { background: var(--panel); border: 1px solid var(--line); border-radius: 10px; padding: 14px 16px; }
.tile .v { font-size: 24px; font-weight: 600; font-variant-numeric: tabular-nums; }
.wrap { background: var(--panel); border: 1px solid var(--line); border-radius: 10px; overflow-x: auto; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 9px 12px; border-bottom: 1px solid var(--line); white-space: nowrap; }
th { font-weight: 500; color: var(--muted); font-size: 12px; }
tr:last-child td { border-bottom: 0; }
td.num { font-variant-numeric: tabular-nums; }
td.cmd { font-family: ui-monospace, Consolas, monospace; font-size: 12.5px; white-space: normal; word-break: break-all; }
.pill { display: inline-block; padding: 1px 9px; border-radius: 999px; font-size: 12px; font-weight: 500; }
.ready { color: var(--ok); background: var(--ok-bg); }
.busy { color: var(--bar); background: var(--idle-bg); }
.paused { color: var(--warn); background: var(--warn-bg); }
.refused, .fail { color: var(--bad); background: var(--bad-bg); }
.no-agent { color: var(--idle); background: var(--idle-bg); }
.bar { width: 90px; height: 6px; background: var(--line); border-radius: 3px; display: inline-block; vertical-align: middle; margin-right: 8px; }
.bar > i { display: block; height: 100%; background: var(--bar); border-radius: 3px; }
.empty { padding: 16px 12px; color: var(--muted); }
#err { color: var(--bad); }
</style>
</head>
<body>
<main>
  <header>
    <h1>Compute pool</h1>
    <span class="muted"><span id="err"></span> <span id="updated">Loading…</span></span>
  </header>
  <div class="tiles">
    <div class="tile"><div class="muted">Devices ready</div><div class="v" id="t-ready">–</div></div>
    <div class="tile"><div class="muted">Idle CPUs (ready devices)</div><div class="v" id="t-idle">–</div></div>
    <div class="tile"><div class="muted">CPUs in the pool</div><div class="v" id="t-cpus">–</div></div>
    <div class="tile"><div class="muted">Jobs running</div><div class="v" id="t-running">–</div></div>
  </div>
  <h2>Devices</h2>
  <div class="wrap"><table>
    <thead><tr><th>Device</th><th>Status</th><th>Load</th><th>Idle CPUs</th><th>Jobs</th><th>Power</th><th>Version</th></tr></thead>
    <tbody id="devices"></tbody>
  </table></div>
  <h2>Running jobs</h2>
  <div class="wrap" id="running"></div>
  <h2>Recent jobs</h2>
  <div class="wrap" id="recent"></div>
</main>
<script>
"use strict";
const $ = (id) => document.getElementById(id);
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}
function row(cells) {
  const tr = el("tr");
  for (const c of cells) {
    const td = c instanceof Node ? c : el("td", null, c);
    tr.appendChild(td.tagName === "TD" ? td : wrapTd(td));
  }
  return tr;
}
function wrapTd(node, cls) { const td = el("td", cls); td.appendChild(node); return td; }
function ago(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return Math.round(s) + "s ago";
  if (s < 3600) return Math.round(s / 60) + "m ago";
  return Math.round(s / 3600) + "h ago";
}
function dur(ms) {
  if (ms < 1000) return ms + " ms";
  if (ms < 60000) return (ms / 1000).toFixed(1) + " s";
  return Math.floor(ms / 60000) + "m " + Math.round((ms % 60000) / 1000) + "s";
}
function power(i) {
  if (!i.has_battery) return "AC";
  const pct = i.battery_pct >= 0 ? " " + i.battery_pct + "%" : "";
  return (i.on_battery ? "Battery" : "AC") + pct;
}
const STATUS = { "ready": "Ready", "busy": "Busy", "paused": "Paused", "refused": "Refused", "no-agent": "No agent" };
function renderDevices(devs) {
  const body = $("devices");
  body.replaceChildren();
  for (const d of devs) {
    const name = el("td", null, d.name + (d.self ? " (this device)" : ""));
    const pill = el("span", "pill " + d.status, STATUS[d.status] || d.status);
    const st = el("td"); st.appendChild(pill);
    if (d.detail) st.appendChild(el("span", "muted", " " + d.detail));
    const i = d.info;
    if (!i) { body.appendChild(row([name, st, "–", "–", "–", "–", "–"])); continue; }
    const load = el("td", "num");
    if (i.cpu_busy_pct >= 0) {
      const bar = el("span", "bar"); const fill = el("i");
      fill.style.width = Math.min(100, i.cpu_busy_pct).toFixed(0) + "%";
      bar.appendChild(fill); load.appendChild(bar);
      load.appendChild(document.createTextNode(i.cpu_busy_pct.toFixed(0) + "%"));
    } else load.textContent = "unknown";
    body.appendChild(row([name, st, load,
      el("td", "num", i.idle_cpus.toFixed(1) + " / " + i.logical_cpus),
      el("td", "num", i.running + " / " + i.max_jobs), power(i), i.version]));
  }
}
function jobTable(target, jobs, running) {
  const box = $(target);
  box.replaceChildren();
  if (!jobs.length) { box.appendChild(el("div", "empty", running ? "Nothing running." : "No jobs yet.")); return; }
  const t = el("table"), head = el("thead"), body = el("tbody");
  const hr = el("tr");
  for (const h of running ? ["Device", "From", "Command", "Started"] : ["Device", "From", "Command", "Finished", "Took", "Result"]) {
    hr.appendChild(el("th", null, h));
  }
  head.appendChild(hr);
  for (const j of jobs) {
    const cmd = el("td", "cmd", j.command);
    if (running) { body.appendChild(row([j.device, j.from, cmd, ago(j.started)])); continue; }
    const ok = j.code === 0 && !j.error;
    const res = el("td"); res.appendChild(el("span", "pill " + (ok ? "ready" : "fail"), ok ? "exit 0" : "exit " + j.code));
    if (j.error) res.appendChild(el("span", "muted", " " + j.error));
    const finished = new Date(new Date(j.started).getTime() + (j.ms || 0)).toISOString();
    body.appendChild(row([j.device, j.from, cmd, ago(finished), el("td", "num", dur(j.ms || 0)), res]));
  }
  t.appendChild(head); t.appendChild(body); box.appendChild(t);
}
async function refresh() {
  try {
    const r = await fetch("/api/state", { cache: "no-store" });
    if (!r.ok) throw new Error(await r.text());
    const s = await r.json();
    $("t-ready").textContent = s.ready + " / " + s.devices.filter(d => d.info).length;
    $("t-idle").textContent = s.idle_cpus.toFixed(1);
    $("t-cpus").textContent = s.total_cpus;
    $("t-running").textContent = s.running.length;
    renderDevices(s.devices);
    jobTable("running", s.running, true);
    jobTable("recent", s.recent, false);
    $("err").textContent = "";
    $("updated").textContent = "Updated " + new Date(s.updated).toLocaleTimeString();
  } catch (e) {
    $("err").textContent = "Can't reach the pool: " + e.message + " ·";
  }
}
refresh();
setInterval(refresh, 2000);
</script>
</body>
</html>
`
