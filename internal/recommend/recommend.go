// Package recommend turns a device's specs and measured speed into agent
// settings, with a reason for each, for pool setup to show before applying.
package recommend

import (
	"fmt"
	"strings"

	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/proto"
)

const (
	// kneeShare: lend up to the fewest jobs that reach this share of the
	// device's peak measured throughput. Beyond it, extra jobs add little
	// speed but still take CPU from the owner.
	kneeShare = 0.9
	// DefaultJobMemoryMB is the memory assumed per job when the caller
	// doesn't say.
	DefaultJobMemoryMB = 512
	// DefaultBusyThreshold matches pool agent's default.
	DefaultBusyThreshold = 80
)

// Input is what the rules look at.
type Input struct {
	Specs      hwinfo.Specs
	PowerKnown bool
	HasBattery bool
	// Capacity is the device's measured speed, or nil if not measured.
	Capacity *proto.Capacity
	// BaselineBusyPct is how busy the CPU was just before measuring, or -1.
	BaselineBusyPct float64
	// KeepThreads is how many hardware threads to leave for the owner; 0
	// means a quarter of them, at least 2.
	KeepThreads int
	// JobMemoryMB is the memory each job is expected to need; 0 means
	// DefaultJobMemoryMB.
	JobMemoryMB int64
}

// Setting is one recommended agent setting, as shown to the user.
type Setting struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

// Result is the recommendation.
type Result struct {
	MaxJobs       int     `json:"max_jobs"`
	LendOnBattery bool    `json:"lend_on_battery"`
	BusyThreshold float64 `json:"busy_threshold"`
	// Speedup is the measured throughput at MaxJobs relative to one job, or
	// 0 when unmeasured.
	Speedup  float64   `json:"speedup,omitempty"`
	Settings []Setting `json:"settings"`
	Warnings []string  `json:"warnings,omitempty"`
}

// Recommend applies the rules.
func Recommend(in Input) Result {
	s := in.Specs
	logical := max(1, s.LogicalCPUs)
	keep := in.KeepThreads
	if keep <= 0 {
		keep = max(2, logical/4)
	}
	jobMem := in.JobMemoryMB
	if jobMem <= 0 {
		jobMem = DefaultJobMemoryMB
	}

	r := Result{BusyThreshold: DefaultBusyThreshold}

	// Start from what the measurement says the CPU can use well, then apply
	// the limits that protect the owner and the machine.
	jobs, why := baseJobs(in, logical)
	if limit := max(1, logical-keep); jobs > limit {
		jobs, why = limit, fmt.Sprintf("leaves %s of %d free for your own work", plural(logical-limit, "thread"), logical)
	}
	if s.MemoryMB > 0 {
		reserve := max(1024, s.MemoryMB/4)
		if limit := max(1, int((s.MemoryMB-reserve)/jobMem)); jobs > limit {
			jobs, why = limit, fmt.Sprintf("limited by memory: %.1f GB, keeping %.1f GB for the system, at %d MB per job",
				float64(s.MemoryMB)/1024, float64(reserve)/1024, jobMem)
		}
	}
	r.MaxJobs = jobs
	if single := in.Capacity.RateAt(1); single > 0 {
		r.Speedup = in.Capacity.RateAt(jobs) / single
		why += fmt.Sprintf(" (about %.1f× the speed of one job)", r.Speedup)
	}
	r.Settings = append(r.Settings, Setting{"max jobs", fmt.Sprint(jobs), why})

	if in.HasBattery {
		r.Settings = append(r.Settings, Setting{"lend on battery", "no",
			"on battery a laptop slows down and drains; in tests pooling one on battery gained only 0.98-1.14×"})
	}
	r.Settings = append(r.Settings, Setting{"busy threshold", fmt.Sprintf("%.0f%%", r.BusyThreshold),
		"new jobs wait while your own work keeps the CPU this busy"})

	if in.Capacity != nil && in.Capacity.OnBattery {
		r.Warnings = append(r.Warnings, "measured on battery, so the speeds are lower than when plugged in; run pool setup again while plugged in")
	}
	if in.BaselineBusyPct >= 25 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("the CPU was %.0f%% busy before measuring, so the speeds may be low; run pool setup again when the device is idle", in.BaselineBusyPct))
	}
	if !in.PowerKnown {
		r.Warnings = append(r.Warnings, "couldn't tell whether this device has a battery")
	}
	return r
}

// baseJobs is the number of jobs the CPU itself supports, before limits.
func baseJobs(in Input, logical int) (int, string) {
	c := in.Capacity
	if peak := c.Peak(); peak.Rate > 0 {
		for _, l := range c.Levels {
			if l.Jobs <= logical && l.Rate >= kneeShare*peak.Rate {
				return l.Jobs, fmt.Sprintf("%s reach %.0f%% of this device's top measured speed; more add little",
					plural(l.Jobs, "job"), 100*l.Rate/peak.Rate)
			}
		}
	}
	if n := in.Specs.PhysicalCores; n > 0 {
		return n, "not measured: one job per physical core"
	}
	return max(1, logical/2), "not measured: one job per two hardware threads"
}

// Args returns the pool install (or pool agent) flags for r and allow.
func (r Result) Args(allow []string) []string {
	var args []string
	if len(allow) > 0 {
		args = append(args, "--allow", strings.Join(allow, ","))
	}
	args = append(args, "--max-jobs", fmt.Sprint(r.MaxJobs))
	if r.LendOnBattery {
		args = append(args, "--lend-on-battery")
	}
	if r.BusyThreshold != DefaultBusyThreshold {
		args = append(args, "--busy-threshold", fmt.Sprintf("%g", r.BusyThreshold))
	}
	return args
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
