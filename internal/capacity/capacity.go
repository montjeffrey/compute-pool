// Package capacity measures how much CPU work a device gets through with
// different numbers of jobs running at once, and keeps the result on disk so
// the agent can report it to the rest of the pool.
package capacity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/proto"
)

// Kernel identifies the work unit timed by Measure. Change it whenever the
// work unit changes, so old and new measurements aren't compared.
const Kernel = "primes-300k-v1"

// workLimit sets the size of one work unit: counting primes below it by
// trial division takes about 10 ms on a current desktop core.
const workLimit = 300_000

// Sink keeps the compiler from discarding the work.
var Sink int

func workUnit() int {
	n := 1 // 2
	for c := 3; c < workLimit; c += 2 {
		prime := true
		for d := 3; d*d <= c; d += 2 {
			if c%d == 0 {
				prime = false
				break
			}
		}
		if prime {
			n++
		}
	}
	return n
}

// Levels chooses which numbers of parallel jobs to measure: powers of two
// up to the logical CPU count, plus the performance-core, physical-core and
// logical-CPU counts, where throughput tends to change slope.
func Levels(s hwinfo.Specs) []int {
	logical := max(1, s.LogicalCPUs)
	set := map[int]bool{logical: true}
	for n := 1; n < logical; n *= 2 {
		set[n] = true
	}
	for _, n := range []int{s.PerfCores, s.PhysicalCores} {
		if n > 0 && n <= logical {
			set[n] = true
		}
	}
	levels := make([]int, 0, len(set))
	for n := range set {
		levels = append(levels, n)
	}
	sort.Ints(levels)
	return levels
}

// Measure runs the work unit with each number of parallel jobs in levels for
// about per, after a short warm-up, and reports each result to progress (if
// non-nil) as it is measured. It keeps every CPU busy while it runs.
func Measure(ctx context.Context, levels []int, per time.Duration, progress func(proto.Level)) (proto.Capacity, error) {
	c := proto.Capacity{Kernel: Kernel, MeasuredAt: time.Now().UTC()}
	if len(levels) == 0 {
		return c, errors.New("no levels to measure")
	}
	// Let the CPU raise its clock before the first timed run.
	if _, err := run(ctx, 1, per/4); err != nil {
		return c, err
	}
	for _, jobs := range levels {
		rate, err := run(ctx, jobs, per)
		if err != nil {
			return c, err
		}
		l := proto.Level{Jobs: jobs, Rate: rate}
		c.Levels = append(c.Levels, l)
		if progress != nil {
			progress(l)
		}
	}
	return c, nil
}

// run keeps jobs goroutines doing work units for d and returns the combined
// rate. Each goroutine's rate is its own units over its own elapsed time, so
// the unit in progress when time runs out is counted fairly.
func run(ctx context.Context, jobs int, d time.Duration) (float64, error) {
	if jobs < 1 {
		return 0, fmt.Errorf("invalid level %d", jobs)
	}
	deadline := time.Now().Add(d)
	rates := make([]float64, jobs)
	results := make([]int, jobs)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start, units, acc := time.Now(), 0, 0
			for time.Now().Before(deadline) && ctx.Err() == nil {
				acc += workUnit()
				units++
			}
			rates[i] = float64(units) / time.Since(start).Seconds()
			results[i] = acc
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	total := 0.0
	for i, r := range rates {
		total += r
		Sink += results[i]
	}
	return total, nil
}

// DefaultPath is where pool setup saves the measurement and the agent reads
// it: next to the agent's job and cache folders.
func DefaultPath() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "compute-pool", "capacity.json")
}

// Save writes c to path.
func Save(path string, c proto.Capacity) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a measurement saved by Save. It returns an error for a file
// that is missing, unreadable or from a different kernel.
func Load(path string) (*proto.Capacity, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c proto.Capacity
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if c.Kernel != Kernel {
		return nil, fmt.Errorf("%s was measured with %q, not %q; run pool setup again", path, c.Kernel, Kernel)
	}
	if len(c.Levels) == 0 {
		return nil, fmt.Errorf("%s has no measurements", path)
	}
	return &c, nil
}
