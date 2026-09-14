package upstream

import (
	"sync"
	"time"
)

type BreakerState int

const (
	StateClosed BreakerState = iota
	StateOpen
	StateHalfOpen
)

func (s BreakerState) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// Breaker is a per-upstream circuit breaker: it opens after N consecutive
// failures or a failure rate above a threshold within a trailing window,
// half-opens after a cooldown to admit a single probe, and closes on that
// probe's success.
type Breaker struct {
	mu sync.Mutex

	consecutiveFailThreshold int
	failureRateThreshold     float64
	rateWindow               time.Duration
	cooldown                 time.Duration
	nowFunc                  func() time.Time

	state           BreakerState
	consecutiveFail int
	openedAt        time.Time
	halfOpenInFlight bool

	events []event // trailing window of pass/fail for rate calc
}

type event struct {
	at      time.Time
	success bool
}

type BreakerConfig struct {
	ConsecutiveFailThreshold int
	FailureRateThreshold     float64 // e.g. 0.5 = open at 50% failure rate
	RateWindow               time.Duration
	Cooldown                 time.Duration
}

func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		ConsecutiveFailThreshold: 5,
		FailureRateThreshold:     0.5,
		RateWindow:               1 * time.Minute,
		Cooldown:                 30 * time.Second,
	}
}

func NewBreaker(cfg BreakerConfig) *Breaker {
	return &Breaker{
		consecutiveFailThreshold: cfg.ConsecutiveFailThreshold,
		failureRateThreshold:     cfg.FailureRateThreshold,
		rateWindow:               cfg.RateWindow,
		cooldown:                 cfg.Cooldown,
		nowFunc:                  time.Now,
		state:                    StateClosed,
	}
}

// SetNowFunc overrides the clock. Test-only hook.
func (b *Breaker) SetNowFunc(f func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nowFunc = f
}

// Allow reports whether a request may be dispatched to this upstream right
// now, and if so, whether it is the single half-open probe.
func (b *Breaker) Allow() (allowed bool, isProbe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.nowFunc()
	switch b.state {
	case StateClosed:
		return true, false
	case StateOpen:
		if now.Sub(b.openedAt) >= b.cooldown {
			b.state = StateHalfOpen
			b.halfOpenInFlight = true
			return true, true
		}
		return false, false
	case StateHalfOpen:
		if !b.halfOpenInFlight {
			b.halfOpenInFlight = true
			return true, true
		}
		return false, false
	}
	return false, false
}

// RecordResult reports the outcome of a dispatched request.
// onClose is invoked (outside the lock) exactly when the breaker transitions
// into StateClosed, so the caller can reset drift-debt bookkeeping.
func (b *Breaker) RecordResult(success bool, onClose func()) {
	b.mu.Lock()
	now := b.nowFunc()
	b.events = appendPruned(b.events, event{at: now, success: success}, now.Add(-b.rateWindow))

	transitioned := false
	switch b.state {
	case StateHalfOpen:
		b.halfOpenInFlight = false
		if success {
			b.state = StateClosed
			b.consecutiveFail = 0
			b.events = nil
			transitioned = true
		} else {
			b.state = StateOpen
			b.openedAt = now
			b.consecutiveFail = 0
		}
	default: // Closed or Open (a stray result racing a state change)
		if success {
			b.consecutiveFail = 0
		} else {
			b.consecutiveFail++
			if b.state == StateClosed && b.shouldOpenLocked() {
				b.state = StateOpen
				b.openedAt = now
			}
		}
	}
	b.mu.Unlock()

	if transitioned && onClose != nil {
		onClose()
	}
}

func (b *Breaker) shouldOpenLocked() bool {
	if b.consecutiveFail >= b.consecutiveFailThreshold {
		return true
	}
	if len(b.events) < 5 {
		return false // not enough samples to judge a rate
	}
	var fails int
	for _, e := range b.events {
		if !e.success {
			fails++
		}
	}
	return float64(fails)/float64(len(b.events)) >= b.failureRateThreshold
}

func appendPruned(events []event, e event, cutoff time.Time) []event {
	events = append(events, e)
	i := 0
	for _, ev := range events {
		if ev.at.After(cutoff) {
			events[i] = ev
			i++
		}
	}
	return events[:i]
}

// State returns the current breaker state (for /internal/stats).
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
