package proto

import (
	"math"
	"testing"
)

func TestCapacityRateAtAndPeak(t *testing.T) {
	c := &Capacity{Levels: []Level{{Jobs: 1, Rate: 100}, {Jobs: 4, Rate: 340}, {Jobs: 8, Rate: 520}, {Jobs: 16, Rate: 500}}}
	cases := map[int]float64{
		0:  0,
		1:  100,
		2:  180, // a third of the way from 1 (100) to 4 (340)
		4:  340,
		6:  430,
		16: 500,
		32: 500, // beyond the last level
	}
	for jobs, want := range cases {
		if got := c.RateAt(jobs); math.Abs(got-want) > 1e-9 {
			t.Errorf("RateAt(%d) = %v, want %v", jobs, got, want)
		}
	}
	if p := c.Peak(); p.Jobs != 8 || p.Rate != 520 {
		t.Errorf("Peak = %+v", p)
	}
	var none *Capacity
	if none.RateAt(4) != 0 || none.Peak().Rate != 0 {
		t.Error("nil capacity should report nothing")
	}
}
