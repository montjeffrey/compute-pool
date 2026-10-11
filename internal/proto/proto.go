// Package proto defines the wire format between the pool CLI and agents.
//
// A job is submitted with POST /v1/run and a JSON RunRequest body. The agent
// answers with a stream of newline-delimited JSON Frames: one "start" frame,
// any number of "stdout"/"stderr" frames, then any collected "file" frames,
// an optional "error" frame, and a final "exit" frame. A stream that ends without an "exit" frame means the
// connection was lost before the job finished.
package proto

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultPort is the TCP port agents listen on.
	DefaultPort = 7470
	// Version is reported by agents and the CLI.
	Version = "0.1.0-dev"
)

const (
	// MaxRequestBytes caps a RunRequest, including base64-encoded input
	// files (roughly 190 MiB of raw file data).
	MaxRequestBytes = 256 << 20
	// MaxCollectBytes caps the files an agent sends back for one job.
	MaxCollectBytes = 256 << 20
	// FileChunkBytes is the most file data carried by one frame.
	FileChunkBytes = 1 << 20
)

// RunRequest asks an agent to run one command.
type RunRequest struct {
	// Argv is the program and its arguments. With Shell set, the elements are
	// joined with spaces and handed to the agent's platform shell. When
	// Argv[0] names a shipped file, that file is what runs.
	Argv       []string          `json:"argv"`
	Shell      bool              `json:"shell,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	TimeoutSec int               `json:"timeout_sec,omitempty"`

	// Files are written into the job's scratch directory before it starts.
	Files []InputFile `json:"files,omitempty"`
	// Collect lists glob patterns, relative to the scratch directory, whose
	// matches are sent back after the job exits. A matching directory is
	// sent with everything under it.
	Collect []string `json:"collect,omitempty"`
}

// InputFile is a file shipped with a job. Path is slash-separated and
// relative to the job's scratch directory.
//
// When SHA256 is set the agent verifies Data against it and caches the file.
// A later request may then set Cached and omit Data; if the agent no longer
// has the file it answers 412 with MissingFiles and the client resends.
type InputFile struct {
	Path   string `json:"path"`
	Data   []byte `json:"data,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Cached bool   `json:"cached,omitempty"`
}

// MissingFiles is the body of a 412 response: content hashes the agent was
// asked to take from its cache but does not have.
type MissingFiles struct {
	Missing []string `json:"missing"`
}

// CleanRelPath normalizes p to a slash-separated relative path and rejects
// anything that could escape the directory it is joined to: absolute paths,
// "..", drive letters and other colons.
func CleanRelPath(p string) (string, error) {
	p = strings.ReplaceAll(p, `\`, "/")
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, ":") {
		return "", fmt.Errorf("unsafe path %q", p)
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("unsafe path %q", p)
	}
	return c, nil
}

// FrameType identifies what a Frame carries.
type FrameType string

const (
	FrameStart  FrameType = "start"
	FrameStdout FrameType = "stdout"
	FrameStderr FrameType = "stderr"
	FrameError  FrameType = "error"
	FrameExit   FrameType = "exit"
	// FrameFile carries a chunk of a collected output file. The first frame
	// for a Path creates the file; later ones append to it.
	FrameFile FrameType = "file"
)

// Frame is one message in a job's output stream.
type Frame struct {
	Type       FrameType `json:"t"`
	Data       []byte    `json:"d,omitempty"`
	Path       string    `json:"path,omitempty"`
	JobID      string    `json:"job,omitempty"`
	Node       string    `json:"node,omitempty"`
	ExitCode   int       `json:"code,omitempty"`
	DurationMS int64     `json:"ms,omitempty"`
	Message    string    `json:"msg,omitempty"`
}

// Info describes an agent and its current load.
type Info struct {
	Node        string    `json:"node"`
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	LogicalCPUs int       `json:"logical_cpus"`
	MaxJobs     int       `json:"max_jobs"`
	Running     int       `json:"running"`
	Version     string    `json:"version"`
	StartedAt   time.Time `json:"started_at"`

	// CPUBusyPct is system-wide CPU use (0-100), or -1 if unknown.
	CPUBusyPct float64 `json:"cpu_busy_pct"`
	// IdleCPUs estimates how many logical CPUs are free right now.
	IdleCPUs   float64 `json:"idle_cpus"`
	HasBattery bool    `json:"has_battery,omitempty"`
	OnBattery  bool    `json:"on_battery,omitempty"`
	BatteryPct int     `json:"battery_pct"` // -1 if unknown or no battery

	// Accepting reports whether a job sent now would be taken. When false,
	// Reason says why (on battery, CPU busy, all slots in use).
	Accepting bool   `json:"accepting"`
	Reason    string `json:"reason,omitempty"`
}

// JobRecord describes a job an agent is running or recently ran.
type JobRecord struct {
	ID         string    `json:"id"`
	From       string    `json:"from"` // device that submitted it
	Command    string    `json:"command"`
	Started    time.Time `json:"started"`
	Running    bool      `json:"running"`
	DurationMS int64     `json:"ms,omitempty"`
	ExitCode   int       `json:"code"`
	Error      string    `json:"error,omitempty"`
}

// Jobs is the answer to GET /v1/jobs, newest first.
type Jobs struct {
	Running []JobRecord `json:"running"`
	Recent  []JobRecord `json:"recent"`
}

// FrameWriter serializes frames onto a stream. It is safe for concurrent use,
// which matters because stdout and stderr are copied by separate goroutines.
type FrameWriter struct {
	mu    sync.Mutex
	enc   *json.Encoder
	flush func()
	err   error
}

// NewFrameWriter returns a FrameWriter on w. flush, if non-nil, is called
// after every frame so output reaches the client immediately.
func NewFrameWriter(w io.Writer, flush func()) *FrameWriter {
	return &FrameWriter{enc: json.NewEncoder(w), flush: flush}
}

// Write sends one frame. After the first failure every call returns that error.
func (fw *FrameWriter) Write(f Frame) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.err != nil {
		return fw.err
	}
	if err := fw.enc.Encode(f); err != nil {
		fw.err = err
		return err
	}
	if fw.flush != nil {
		fw.flush()
	}
	return nil
}

// Stream returns an io.Writer that wraps everything written to it in frames of
// type t.
func (fw *FrameWriter) Stream(t FrameType) io.Writer {
	return streamWriter{fw: fw, t: t}
}

type streamWriter struct {
	fw *FrameWriter
	t  FrameType
}

func (s streamWriter) Write(p []byte) (int, error) {
	// Encode serializes p before returning, so the caller may reuse it.
	if err := s.fw.Write(Frame{Type: s.t, Data: p}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// FrameReader decodes frames from a stream.
type FrameReader struct {
	dec *json.Decoder
}

// NewFrameReader returns a FrameReader on r.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{dec: json.NewDecoder(r)}
}

// Next returns the next frame, or io.EOF at the end of the stream.
func (fr *FrameReader) Next() (Frame, error) {
	var f Frame
	err := fr.dec.Decode(&f)
	return f, err
}
