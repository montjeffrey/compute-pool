package capacity

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/proto"
)

func TestLevels(t *testing.T) {
	cases := []struct {
		specs hwinfo.Specs
		want  []int
	}{
		{hwinfo.Specs{LogicalCPUs: 20, PhysicalCores: 12, PerfCores: 8, EffCores: 4}, []int{1, 2, 4, 8, 12, 16, 20}},
		{hwinfo.Specs{LogicalCPUs: 16, PhysicalCores: 8}, []int{1, 2, 4, 8, 16}},
		{hwinfo.Specs{LogicalCPUs: 6, PhysicalCores: 6}, []int{1, 2, 4, 6}},
		{hwinfo.Specs{LogicalCPUs: 1}, []int{1}},
		{hwinfo.Specs{}, []int{1}},
	}
	for _, c := range cases {
		if got := Levels(c.specs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Levels(%+v) = %v, want %v", c.specs, got, c.want)
		}
	}
}

func TestMeasure(t *testing.T) {
	var seen []int
	c, err := Measure(context.Background(), []int{1, 2}, 100*time.Millisecond, func(l proto.Level) { seen = append(seen, l.Jobs) })
	if err != nil {
		t.Fatal(err)
	}
	if c.Kernel != Kernel || c.MeasuredAt.IsZero() || len(c.Levels) != 2 {
		t.Fatalf("capacity = %+v", c)
	}
	for _, l := range c.Levels {
		if l.Rate <= 0 {
			t.Errorf("level %d: rate %v", l.Jobs, l.Rate)
		}
	}
	if !reflect.DeepEqual(seen, []int{1, 2}) {
		t.Errorf("progress saw %v", seen)
	}
	t.Logf("1 job: %.0f units/s; 2 jobs: %.0f units/s", c.Levels[0].Rate, c.Levels[1].Rate)
}

func TestMeasureStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Measure(ctx, []int{1}, time.Minute, nil); err == nil {
		t.Error("canceled measurement returned no error")
	}
	if _, err := Measure(context.Background(), nil, time.Millisecond, nil); err == nil {
		t.Error("measurement without levels returned no error")
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "capacity.json")
	want := proto.Capacity{Kernel: Kernel, MeasuredAt: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC),
		Levels: []proto.Level{{Jobs: 1, Rate: 90}, {Jobs: 4, Rate: 340}}}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("Load = %+v, %v", got, err)
	}

	other := want
	other.Kernel = "old-kernel"
	if err := Save(path, other); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("loaded a measurement from a different kernel")
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("loaded a corrupt file")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("loaded a missing file")
	}
}
