package upstream

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	b := NewBreaker(BreakerConfig{ConsecutiveFailThreshold: 3, FailureRateThreshold: 0.99, RateWindow: time.Minute, Cooldown: time.Second})
	for i := 0; i < 2; i++ {
		allowed, _ := b.Allow()
		require.True(t, allowed)
		b.RecordResult(false, nil)
	}
	require.Equal(t, StateClosed, b.State())

	allowed, _ := b.Allow()
	require.True(t, allowed)
	b.RecordResult(false, nil)
	require.Equal(t, StateOpen, b.State())

	allowed, _ = b.Allow()
	require.False(t, allowed, "an open breaker must refuse requests before cooldown elapses")
}

func TestBreaker_HalfOpenAdmitsSingleProbe(t *testing.T) {
	now := time.Now()
	clock := &fakeClock{t: now}
	b := NewBreaker(BreakerConfig{ConsecutiveFailThreshold: 1, FailureRateThreshold: 0.99, RateWindow: time.Minute, Cooldown: 5 * time.Second})
	b.SetNowFunc(clock.Now)

	allowed, _ := b.Allow()
	require.True(t, allowed)
	b.RecordResult(false, nil)
	require.Equal(t, StateOpen, b.State())

	clock.Advance(6 * time.Second)
	allowed1, probe1 := b.Allow()
	require.True(t, allowed1)
	require.True(t, probe1)

	// A second concurrent request must not also be admitted as a probe.
	allowed2, _ := b.Allow()
	require.False(t, allowed2)
}

func TestBreaker_ClosesOnProbeSuccessAndInvokesOnClose(t *testing.T) {
	now := time.Now()
	clock := &fakeClock{t: now}
	b := NewBreaker(BreakerConfig{ConsecutiveFailThreshold: 1, FailureRateThreshold: 0.99, RateWindow: time.Minute, Cooldown: time.Second})
	b.SetNowFunc(clock.Now)

	b.Allow()
	b.RecordResult(false, nil)
	require.Equal(t, StateOpen, b.State())

	clock.Advance(2 * time.Second)
	b.Allow() // admits the probe

	closed := false
	b.RecordResult(true, func() { closed = true })
	require.Equal(t, StateClosed, b.State())
	require.True(t, closed, "onClose must fire exactly when the breaker transitions to Closed")
}

func TestBreaker_HalfOpenFailureReopens(t *testing.T) {
	now := time.Now()
	clock := &fakeClock{t: now}
	b := NewBreaker(BreakerConfig{ConsecutiveFailThreshold: 1, FailureRateThreshold: 0.99, RateWindow: time.Minute, Cooldown: time.Second})
	b.SetNowFunc(clock.Now)

	b.Allow()
	b.RecordResult(false, nil)
	clock.Advance(2 * time.Second)
	b.Allow()
	b.RecordResult(false, nil)
	require.Equal(t, StateOpen, b.State())
}

func TestBreaker_FailureRateOpensWithoutConsecutiveStreak(t *testing.T) {
	b := NewBreaker(BreakerConfig{ConsecutiveFailThreshold: 100, FailureRateThreshold: 0.5, RateWindow: time.Minute, Cooldown: time.Second})
	// Alternate success/failure so consecutive-failure count never exceeds 1,
	// but the rate crosses 50%.
	for i := 0; i < 10; i++ {
		b.Allow()
		b.RecordResult(i%2 == 0, nil)
	}
	require.Equal(t, StateOpen, b.State())
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time  { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }
