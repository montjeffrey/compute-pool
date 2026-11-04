// Package advise looks at every device in the pool at once: how much each
// contributes, how fast the pool is compared with this device alone, and
// which devices are worth lending more or less of.
package advise

import (
	"fmt"
	"strings"

	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/recommend"
)

// smallShare is the share of pool capacity below which a device is called
// out as adding little.
const smallShare = 0.10

// Device is one device as seen from this one.
type Device struct {
	Name string
	Self bool
	// Err is set when the device's agent couldn't be reached; the other
	// fields are then unused.
	Err        error
	Version    string
	MaxJobs    int
	Accepting  bool
	Reason     string // why it isn't accepting
	PowerKnown bool
	HasBattery bool
	OnBattery  bool
	Specs      *hwinfo.Specs
	Capacity   *proto.Capacity
}

// Row is the analysis of one device.
type Row struct {
	Name   string `json:"name"`
	Self   bool   `json:"self,omitempty"`
	CPU    string `json:"cpu,omitempty"`
	Lends  int    `json:"lends"`
	Status string `json:"status"`
	// Capacity is the device's throughput at its current settings, in units
	// of one job on the reference device; 0 when unknown.
	Capacity    float64  `json:"capacity,omitempty"`
	Share       float64  `json:"share,omitempty"`       // of the pool's capacity
	Recommended int      `json:"recommended,omitempty"` // max jobs pool setup would choose
	Advice      []string `json:"advice,omitempty"`
}

// Report is the whole analysis.
type Report struct {
	// Reference is the device whose single job is the unit of capacity.
	Reference string `json:"reference"`
	Rows      []Row  `json:"devices"`
	// Total is the capacity of every measured device at current settings;
	// Available leaves out devices that are paused right now.
	Total     float64 `json:"total"`
	Available float64 `json:"available"`
	// SelfCapacity is this device's own capacity at current settings, and
	// Speedup the pool's Total relative to it (0 if unknown).
	SelfCapacity float64 `json:"self_capacity,omitempty"`
	Speedup      float64 `json:"speedup,omitempty"`
	// RecommendedTotal is Total with every measured device on pool setup's
	// recommended settings, and RecommendedSpeedup is relative to this
	// device on its recommended settings.
	RecommendedTotal   float64  `json:"recommended_total,omitempty"`
	RecommendedSpeedup float64  `json:"recommended_speedup,omitempty"`
	Unmeasured         []string `json:"unmeasured,omitempty"`
}

// Analyze builds the report.
func Analyze(devs []Device) Report {
	rep := Report{Rows: make([]Row, len(devs))}
	ref := reference(devs)
	var refCap *proto.Capacity
	if ref != nil {
		rep.Reference, refCap = ref.Name, ref.Capacity
	}
	for i, d := range devs {
		rep.Rows[i] = describe(d, refCap)
	}
	if ref == nil {
		for i, d := range devs {
			if d.Err == nil {
				rep.Rows[i].Capacity = 0
				rep.Unmeasured = append(rep.Unmeasured, d.Name)
			}
		}
		return rep
	}
	unit := ref.Capacity.RateAt(1)

	type measured struct {
		i               int
		rate, recRate   float64
		lends, recJobs  int
		paused, selfDev bool
	}
	var ms []measured
	for i, d := range devs {
		r := &rep.Rows[i]
		if d.Err != nil {
			continue
		}
		if r.Capacity < 0 {
			r.Capacity = 0
			rep.Unmeasured = append(rep.Unmeasured, d.Name)
			continue
		}
		rec := recommend.Recommend(recommend.Input{
			Specs:           specsOf(d),
			PowerKnown:      d.PowerKnown,
			HasBattery:      d.HasBattery,
			Capacity:        d.Capacity,
			BaselineBusyPct: -1,
		})
		r.Recommended = rec.MaxJobs
		r.Capacity = d.Capacity.RateAt(d.MaxJobs) / unit
		ms = append(ms, measured{
			i: i, rate: r.Capacity, recRate: d.Capacity.RateAt(rec.MaxJobs) / unit,
			lends: d.MaxJobs, recJobs: rec.MaxJobs,
			// A device whose slots are all busy is working for the pool;
			// only battery or owner activity pauses it.
			paused:  !d.Accepting && !strings.HasPrefix(d.Reason, "all "),
			selfDev: d.Self,
		})
	}

	var selfRec float64
	for _, m := range ms {
		rep.Total += m.rate
		rep.RecommendedTotal += m.recRate
		if !m.paused {
			rep.Available += m.rate
		}
		if m.selfDev {
			rep.SelfCapacity, selfRec = m.rate, m.recRate
		}
	}
	if rep.SelfCapacity > 0 {
		rep.Speedup = rep.Total / rep.SelfCapacity
	}
	if selfRec > 0 {
		rep.RecommendedSpeedup = rep.RecommendedTotal / selfRec
	}

	for _, m := range ms {
		r := &rep.Rows[m.i]
		if rep.Total > 0 {
			r.Share = m.rate / rep.Total
		}
		if m.recJobs != m.lends && rep.Total > 0 {
			r.Advice = append(r.Advice, fmt.Sprintf("lends %d jobs; pool setup recommends %d (%+.0f%% pool capacity)",
				m.lends, m.recJobs, 100*(m.recRate-m.rate)/rep.Total))
		}
		if len(ms) > 1 && r.Share < smallShare {
			r.Advice = append(r.Advice, fmt.Sprintf("adds only %.0f%% of the pool's capacity, so it helps mainly with long tasks", 100*r.Share))
		}
		if m.paused && rep.Total > 0 {
			r.Advice = append(r.Advice, fmt.Sprintf("paused now; while it is, the pool has %.0f%% of its capacity", 100*rep.Available/rep.Total))
		}
	}
	return rep
}

// reference picks the device whose single job is the unit: this device if
// it was measured, otherwise the first measured device.
func reference(devs []Device) *Device {
	var first *Device
	for i := range devs {
		d := &devs[i]
		if d.Err != nil || d.Specs == nil || d.Capacity.RateAt(1) <= 0 {
			continue
		}
		if d.Self {
			return d
		}
		if first == nil {
			first = d
		}
	}
	return first
}

// describe fills in a row's identity, status and the advice that doesn't
// depend on the rest of the pool. Capacity is set to -1 for a reachable
// device that can't be compared with ref.
func describe(d Device, ref *proto.Capacity) Row {
	r := Row{Name: d.Name, Self: d.Self, Lends: d.MaxJobs}
	if d.Specs != nil {
		r.CPU = cpuSummary(*d.Specs)
	}
	switch {
	case d.Err != nil:
		r.Status = d.Err.Error()
		return r
	case d.Accepting:
		r.Status = "ready"
	default:
		r.Status = "paused: " + d.Reason
	}
	switch {
	case d.Specs == nil:
		r.Capacity = -1
		r.Advice = append(r.Advice, fmt.Sprintf("runs pool %s, which doesn't report its speed; upgrade it and run pool setup there", d.Version))
	case d.Capacity == nil || len(d.Capacity.Levels) == 0:
		r.Capacity = -1
		r.Advice = append(r.Advice, "not measured yet; run pool setup there")
	case ref != nil && d.Capacity.Kernel != ref.Kernel:
		r.Capacity = -1
		r.Advice = append(r.Advice, "measured by a different pool version; run pool setup there again")
	case d.Capacity.OnBattery:
		r.Advice = append(r.Advice, "measured on battery, so its speed is understated; run pool setup there while plugged in")
	}
	return r
}

func specsOf(d Device) hwinfo.Specs {
	if d.Specs != nil {
		return *d.Specs
	}
	return hwinfo.Specs{}
}

// cpuSummary shortens a CPU name for a table: "i7-12700KF, 12c/20t".
func cpuSummary(s hwinfo.Specs) string {
	model := s.CPUModel
	for _, noise := range []string{"(R)", "(TM)", "(tm)", " CPU", " with Radeon Graphics", "Intel Core", "12th Gen", "13th Gen", "14th Gen"} {
		model = strings.ReplaceAll(model, noise, "")
	}
	model = strings.Join(strings.Fields(model), " ")
	if model == "" {
		model = "unknown CPU"
	}
	if s.PhysicalCores > 0 {
		return fmt.Sprintf("%s, %dc/%dt", model, s.PhysicalCores, s.LogicalCPUs)
	}
	return fmt.Sprintf("%s, %dt", model, s.LogicalCPUs)
}
