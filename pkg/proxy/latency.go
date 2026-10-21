package proxy

import (
	"sync/atomic"
	"time"
)

// LatencyBuckets are the upper bounds of the latency histograms. They span the microseconds the proxy itself should
// add up to the stalls a CPU limit can cause.
var LatencyBuckets = [...]time.Duration{
	10 * time.Microsecond,
	25 * time.Microsecond,
	50 * time.Microsecond,
	100 * time.Microsecond,
	250 * time.Microsecond,
	500 * time.Microsecond,
	time.Millisecond,
	2500 * time.Microsecond,
	5 * time.Millisecond,
	10 * time.Millisecond,
	25 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
}

// Histogram is a snapshot of a latency histogram.
type Histogram struct {
	// Cumulative[i] counts observations no longer than LatencyBuckets[i].
	Cumulative [len(LatencyBuckets)]uint64
	// Count counts every observation, including those above the last bucket.
	Count uint64
	Sum   time.Duration
}

// histogram records durations lock-free, so the data path never blocks on a metrics scrape.
type histogram struct {
	// buckets[i] counts observations in (LatencyBuckets[i-1], LatencyBuckets[i]]; the last one is above every bound.
	buckets [len(LatencyBuckets) + 1]atomic.Uint64
	sum     atomic.Int64
}

func (h *histogram) observe(d time.Duration) {
	// A wall-clock step between the kernel timestamp and now can make d negative.
	d = max(d, 0)
	i := 0
	for i < len(LatencyBuckets) && d > LatencyBuckets[i] {
		i++
	}
	h.buckets[i].Add(1)
	h.sum.Add(int64(d))
}

func (h *histogram) snapshot() Histogram {
	var s Histogram
	for i := range LatencyBuckets {
		s.Count += h.buckets[i].Load()
		s.Cumulative[i] = s.Count
	}
	s.Count += h.buckets[len(LatencyBuckets)].Load()
	s.Sum = time.Duration(h.sum.Load())
	return s
}
