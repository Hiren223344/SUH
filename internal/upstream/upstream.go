// Package upstream models a single configured upstream: its tuned HTTP
// client, circuit breaker, TPM rate bucket, and the sliding-window token
// counters the router package uses for debt-based selection.
package upstream

import (
	"net/http"
	"sync/atomic"
	"time"

	"router/internal/config"
)

// Upstream is the live runtime state for one configured upstream.
type Upstream struct {
	Cfg *config.Upstream

	Breaker *Breaker

	// TokensSent is the sliding window of ACTUAL tokens delivered by this
	// upstream — the basis for realized_share in debt-based selection.
	// Only tokens from responses that were actually returned to the client
	// count; a hop that was tried and failed over does not add here.
	TokensSent *SlidingWindow

	// tpmWindow tracks tokens committed to this upstream (estimate at
	// dispatch time) for TPM rate-limit gating, independent of TokensSent.
	tpmWindow *SlidingWindow

	InFlight atomic.Int64

	Client *http.Client

	// Debt is the incremental deficit-weighted-round-robin accumulator used
	// by router.Select: on every delivered response, every surviving
	// candidate's debt is credited by its (renormalized) target share of
	// the tokens just delivered, and the winner's debt is debited by the
	// full amount. The candidate with the highest debt is the most
	// underserved relative to its target and is selected next. On circuit
	// breaker recovery this is reset to zero so a recovered upstream does
	// not receive a traffic spike to "repay" debt accrued while it was down.
	Debt AtomicFloat64
}

func New(cfg *config.Upstream, bcfg BreakerConfig) *Upstream {
	return &Upstream{
		Cfg:        cfg,
		Breaker:    NewBreaker(bcfg),
		TokensSent: NewSlidingWindow(),
		tpmWindow:  NewSlidingWindow(),
		Client:     newTunedClient(),
	}
}

// newTunedClient builds an http.Client sized for hundreds of concurrent
// long-lived streaming requests to a single host, per the spec's HTTP
// client tuning section. Deliberately no client-level timeout: streaming
// responses use context.WithTimeout on the request budget instead, so a
// healthy long generation is never killed by an idle client deadline.
func newTunedClient() *http.Client {
	// ForceAttemptHTTP2 is sufficient to get HTTP/2-over-TLS via the
	// standard library's bundled support — no golang.org/x/net/http2
	// dependency needed, keeping the binary's dependency list minimal.
	transport := &http.Transport{
		MaxIdleConnsPerHost:   512,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: transport}
}

// ReserveTPM records an estimated token cost against this upstream's TPM
// bucket at dispatch time, ahead of knowing the actual usage.
func (u *Upstream) ReserveTPM(estimate int64) {
	u.tpmWindow.Add(estimate)
}

// HasTPMCapacity reports whether committing another `estimate` tokens would
// stay within the upstream's configured tokens-per-minute limit.
func (u *Upstream) HasTPMCapacity(estimate int64) bool {
	if u.Cfg.TPMLimit <= 0 {
		return true
	}
	return u.tpmWindow.SumLastMinutes(1)+estimate <= u.Cfg.TPMLimit
}

// RecordDelivered adds actually-delivered tokens to the realized-share
// window. Call this on reconciliation (actual usage), not at dispatch time.
func (u *Upstream) RecordDelivered(tokens int64) {
	u.TokensSent.Add(tokens)
}
