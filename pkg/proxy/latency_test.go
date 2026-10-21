package proxy

import (
	"testing"
	"time"
)

func TestHistogramBuckets(t *testing.T) {
	var h histogram
	for _, d := range []time.Duration{
		-time.Millisecond,      // clamped to 0: first bucket
		10 * time.Microsecond,  // on a bound: that bucket
		11 * time.Microsecond,  // second bucket
		3 * time.Millisecond,   // the 5ms bucket
		200 * time.Millisecond, // above every bound
	} {
		h.observe(d)
	}

	s := h.snapshot()
	if s.Count != 5 {
		t.Fatalf("Count = %d, want 5", s.Count)
	}
	if want := 10*time.Microsecond + 11*time.Microsecond + 3*time.Millisecond + 200*time.Millisecond; s.Sum != want {
		t.Errorf("Sum = %s, want %s", s.Sum, want)
	}
	want := map[time.Duration]uint64{
		10 * time.Microsecond:   2,
		25 * time.Microsecond:   3,
		2500 * time.Microsecond: 3,
		5 * time.Millisecond:    4,
		100 * time.Millisecond:  4,
	}
	for i, bound := range LatencyBuckets {
		if w, ok := want[bound]; ok && s.Cumulative[i] != w {
			t.Errorf("Cumulative[le=%s] = %d, want %d", bound, s.Cumulative[i], w)
		}
		if i > 0 && s.Cumulative[i] < s.Cumulative[i-1] {
			t.Errorf("Cumulative[le=%s] = %d decreases from %d", bound, s.Cumulative[i], s.Cumulative[i-1])
		}
	}
}
