package upstream

import (
	"math"
	"sync/atomic"
)

// AtomicFloat64 is a lock-free float64 accumulator (Go's stdlib has no
// atomic.Float64 as of this module's Go version).
type AtomicFloat64 struct {
	bits atomic.Uint64
}

func (a *AtomicFloat64) Load() float64 {
	return math.Float64frombits(a.bits.Load())
}

func (a *AtomicFloat64) Store(v float64) {
	a.bits.Store(math.Float64bits(v))
}

func (a *AtomicFloat64) Add(delta float64) float64 {
	for {
		old := a.bits.Load()
		newV := math.Float64frombits(old) + delta
		newBits := math.Float64bits(newV)
		if a.bits.CompareAndSwap(old, newBits) {
			return newV
		}
	}
}
