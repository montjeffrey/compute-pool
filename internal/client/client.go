// Package client talks to pool agents.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/montjeffrey/compute-pool/internal/proto"
)

// ErrForbidden means the agent refused this device.
var ErrForbidden = errors.New("this device is not allowed by the agent")

// ErrUnreachable means no connection was made to the agent, so nothing ran
// and the job can safely go elsewhere.
var ErrUnreachable = errors.New("agent unreachable")

// ErrBusy means the agent is not taking jobs right now: no free slot, on
// battery, or CPU busy. The wrapping error carries the agent's reason.
var ErrBusy = errors.New("agent not accepting jobs")

// Client calls agents over the tailnet. Traffic is plain HTTP inside
// Tailscale's WireGuard tunnel, which provides encryption and peer identity.
type Client struct {
	HTTP *http.Client
	Port int

	mu    sync.Mutex
	known map[string]map[string]bool // host -> content hashes it has cached
}

func (c *Client) isKnown(host, hash string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.known[host][hash]
}

func (c *Client) markKnown(host string, files []proto.InputFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.known == nil {
		c.known = make(map[string]map[string]bool)
	}
	if c.known[host] == nil {
		c.known[host] = make(map[string]bool)
	}
	for _, f := range files {
		if f.SHA256 != "" {
			c.known[host][f.SHA256] = true
		}
	}
}

// wireRequest replaces the data of files host already has with a cache
// reference, except for hashes listed in resend.
func (c *Client) wireRequest(host string, req proto.RunRequest, resend map[string]bool) proto.RunRequest {
	if len(req.Files) == 0 {
		return req
	}
	files := make([]proto.InputFile, len(req.Files))
	for i, f := range req.Files {
		if f.SHA256 != "" && !resend[f.SHA256] && c.isKnown(host, f.SHA256) {
			f = proto.InputFile{Path: f.Path, SHA256: f.SHA256, Cached: true}
		}
		files[i] = f
	}
	req.Files = files
	return req
}

func (c *Client) post(ctx context.Context, host string, req proto.RunRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(host, "/v1/run"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	return c.do(hreq)
}

// do sends a request, marking failures to get any response as
// ErrUnreachable (unless the caller canceled).
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil && req.Context().Err() == nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return resp, err
}

// Jobs fetches an agent's running and recent jobs.
func (c *Client) Jobs(ctx context.Context, host string) (proto.Jobs, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(host, "/v1/jobs"), nil)
	if err != nil {
		return proto.Jobs{}, err
	}
	resp, err := c.do(req)
	if err != nil {
		return proto.Jobs{}, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return proto.Jobs{}, err
	}
	var jobs proto.Jobs
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
		return proto.Jobs{}, fmt.Errorf("decode jobs: %w", err)
	}
	return jobs, nil
}

// New returns a Client with timeouts suited to long-running streamed jobs:
// connecting and receiving response headers are bounded, the body is not.
func New() *Client {
	return &Client{
		HTTP: &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ResponseHeaderTimeout: 15 * time.Second,
			Proxy:                 nil, // tailnet addresses must never go through a proxy
		}},
		Port: proto.DefaultPort,
	}
}

func (c *Client) url(host, path string) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(c.Port)) + path
}

// Info fetches an agent's description and load.
func (c *Client) Info(ctx context.Context, host string) (proto.Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(host, "/v1/info"), nil)
	if err != nil {
		return proto.Info{}, err
	}
	resp, err := c.do(req)
	if err != nil {
		return proto.Info{}, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return proto.Info{}, err
	}
	var info proto.Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return proto.Info{}, fmt.Errorf("decode info: %w", err)
	}
	return info, nil
}

// Result describes a finished remote job.
type Result struct {
	JobID    string
	Node     string
	ExitCode int
	Duration time.Duration
	// RemoteError is set when the agent, rather than the program, reported a
	// problem: the command could not start, timed out, was canceled, or some
	// requested output files could not be collected.
	RemoteError string
	// Files lists the collected files written under Output.Dir.
	Files []string
}

// Output says where a job's results go.
type Output struct {
	Stdout, Stderr io.Writer
	// Dir receives collected files (RunRequest.Collect). Required if the
	// request collects anything.
	Dir string
}

// Run executes req on the agent at host, copying the job's output as it
// arrives. Canceling ctx makes the agent kill the job.
func (c *Client) Run(ctx context.Context, host string, req proto.RunRequest, out Output) (Result, error) {
	if len(req.Collect) > 0 && out.Dir == "" {
		return Result{}, errors.New("collecting files needs an output directory")
	}
	resp, err := c.post(ctx, host, c.wireRequest(host, req, nil))
	if err != nil {
		return Result{}, err
	}
	if resp.StatusCode == http.StatusPreconditionFailed {
		// The agent's cache lost some files; send those in full this time.
		var mf proto.MissingFiles
		derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&mf)
		resp.Body.Close()
		if derr != nil {
			return Result{}, fmt.Errorf("agent reported missing files: %w", derr)
		}
		resend := make(map[string]bool, len(mf.Missing))
		for _, h := range mf.Missing {
			resend[h] = true
		}
		if resp, err = c.post(ctx, host, c.wireRequest(host, req, resend)); err != nil {
			return Result{}, err
		}
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return Result{}, err
	}
	c.markKnown(host, req.Files)

	var res Result
	files := newFileSink(out.Dir)
	defer files.close()
	fr := proto.NewFrameReader(resp.Body)
	for {
		f, err := fr.Next()
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			return res, fmt.Errorf("connection lost before the job finished: %w", err)
		}
		switch f.Type {
		case proto.FrameStart:
			res.JobID, res.Node = f.JobID, f.Node
		case proto.FrameStdout:
			if _, err := out.Stdout.Write(f.Data); err != nil {
				return res, err
			}
		case proto.FrameStderr:
			if _, err := out.Stderr.Write(f.Data); err != nil {
				return res, err
			}
		case proto.FrameFile:
			if err := files.write(f.Path, f.Data); err != nil {
				return res, err
			}
		case proto.FrameError:
			res.RemoteError = f.Message
		case proto.FrameExit:
			res.ExitCode = f.ExitCode
			res.Duration = time.Duration(f.DurationMS) * time.Millisecond
			res.Files = files.order
			return res, files.close()
		}
	}
}

// fileSink writes collected files under dir. Paths are re-validated here
// because the client must not trust an agent to keep files inside dir.
type fileSink struct {
	dir   string
	open  map[string]*os.File
	order []string
}

func newFileSink(dir string) *fileSink {
	return &fileSink{dir: dir, open: make(map[string]*os.File)}
}

func (s *fileSink) write(p string, data []byte) error {
	if s.dir == "" {
		return errors.New("agent sent files that were not requested")
	}
	rel, err := proto.CleanRelPath(p)
	if err != nil {
		return fmt.Errorf("agent sent %w", err)
	}
	f, ok := s.open[rel]
	if !ok {
		dst := filepath.Join(s.dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if f, err = os.Create(dst); err != nil {
			return err
		}
		s.open[rel] = f
		s.order = append(s.order, rel)
	}
	_, err = f.Write(data)
	return err
}

func (s *fileSink) close() error {
	var first error
	for rel, f := range s.open {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
		delete(s.open, rel)
	}
	return first
}

func checkStatus(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusForbidden:
		return ErrForbidden
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	reason := strings.TrimSpace(string(msg))
	if resp.StatusCode == http.StatusServiceUnavailable {
		return fmt.Errorf("%w: %s", ErrBusy, reason)
	}
	return fmt.Errorf("agent returned %s: %s", resp.Status, reason)
}

// Candidate is a device whose agent answered an Info request.
type Candidate struct {
	Name string // short tailnet name
	Host string // address to dial
	Info proto.Info
}

// Rank returns the candidates that can take a job now, best first: most idle
// CPUs, then the lowest share of job slots in use, then name for a stable
// order. Agents re-check on arrival, so a stale ranking only costs a retry.
func Rank(cs []Candidate) []Candidate {
	var out []Candidate
	for _, c := range cs {
		if c.Info.Accepting && c.Info.Running < c.Info.MaxJobs {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Info, out[j].Info
		if a.IdleCPUs != b.IdleCPUs {
			return a.IdleCPUs > b.IdleCPUs
		}
		la := float64(a.Running) / float64(a.MaxJobs)
		lb := float64(b.Running) / float64(b.MaxJobs)
		if la != lb {
			return la < lb
		}
		return out[i].Name < out[j].Name
	})
	return out
}
