package upstream

import (
	"sync/atomic"
	"time"
)

// slidingWindowBuckets is the number of one-minute buckets kept, per the
// spec's suggestion: 60 buckets gives a rolling 1h view while staying
// responsive to failures within the last few minutes.
const slidingWindowBuckets = 60

// SlidingWindow accumulates token counts into per-minute atomic buckets and
// reports a rolling sum. It never allocates or locks on the hot path.
type SlidingWindow struct {
	buckets   [slidingWindowBuckets]atomic.Int64
	bucketMin [slidingWindowBuckets]atomic.Int64 // unix-minute stamp owning each bucket slot
	nowFunc   func() time.Time
}

func NewSlidingWindow() *SlidingWindow {
	return &SlidingWindow{nowFunc: time.Now}
}

// SetNowFunc overrides the clock used to bucket samples. Test-only hook.
func (w *SlidingWindow) SetNowFunc(f func() time.Time) {
	w.nowFunc = f
}

func (w *SlidingWindow) minute() int64 {
	return w.nowFunc().Unix() / 60
}

func (w *SlidingWindow) slot(minute int64) int {
	i := minute % slidingWindowBuckets
	if i < 0 {
		i += slidingWindowBuckets
	}
	return int(i)
}

// Add records delta tokens against the current minute's bucket, clearing
// any stale bucket that has rolled around from an earlier hour.
func (w *SlidingWindow) Add(delta int64) {
	minute := w.minute()
	idx := w.slot(minute)
	if w.bucketMin[idx].Swap(minute) != minute {
		w.buckets[idx].Store(0)
	}
	w.buckets[idx].Add(delta)
}

// Sum returns the total tokens recorded in the trailing window (default: the
// full 60-minute span covered by the buckets).
func (w *SlidingWindow) Sum() int64 {
	now := w.minute()
	var total int64
	for i := 0; i < slidingWindowBuckets; i++ {
		if w.bucketMin[i].Load() > now-slidingWindowBuckets {
			total += w.buckets[i].Load()
		}
	}
	return total
}

// SumLastMinutes returns the total tokens recorded in the trailing n
// minutes (n <= slidingWindowBuckets).
func (w *SlidingWindow) SumLastMinutes(n int) int64 {
	if n > slidingWindowBuckets {
		n = slidingWindowBuckets
	}
	now := w.minute()
	var total int64
	for i := 0; i < n; i++ {
		minute := now - int64(i)
		idx := w.slot(minute)
		if w.bucketMin[idx].Load() == minute {
			total += w.buckets[idx].Load()
		}
	}
	return total
}
