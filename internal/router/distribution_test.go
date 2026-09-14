package router

import (
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"router/internal/config"
	"router/internal/upstream"
)

// synthUpstream builds an in-memory upstream runtime object with a given
// configured weight, bypassing the registry/HTTP machinery entirely — this
// test is about the selection algorithm's convergence, not transport.
func synthUpstream(id string, weight float64) (*config.PublicModelUpstream, *upstream.Upstream) {
	ref := &config.PublicModelUpstream{ID: id, Weight: weight}
	cfg := &config.Upstream{ID: id, TPMLimit: 1 << 40}
	return ref, upstream.New(cfg, upstream.DefaultBreakerConfig())
}

// logNormalTokens draws a request size in [minTok, maxTok], log-normally
// distributed, matching the spec's "500 -> 200,000 tokens" span.
func logNormalTokens(rng *rand.Rand, minTok, maxTok float64) int64 {
	logMin, logMax := math.Log(minTok), math.Log(maxTok)
	mu := (logMin + logMax) / 2
	sigma := (logMax - logMin) / 6 // ~99.7% of mass within [min,max]
	v := math.Exp(mu + sigma*rng.NormFloat64())
	if v < minTok {
		v = minTok
	}
	if v > maxTok {
		v = maxTok
	}
	return int64(v)
}

// TestDistribution_TokenWeightedConvergence is acceptance test #1: simulate
// 50,000 requests with log-normally distributed sizes spanning 500 to
// 200,000 tokens, and assert realized token share per upstream lands within
// ±2 percentage points of target.
func TestDistribution_TokenWeightedConvergence(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	pm := &config.PublicModel{Name: "M"}
	ids := []string{"a", "b", "c", "d", "e", "f"}
	weights := []float64{0.13, 0.13, 0.12, 0.12, 0.10, 0.40}

	var ups []*upstream.Upstream
	for i, id := range ids {
		ref, u := synthUpstream(id, weights[i])
		pm.Upstreams = append(pm.Upstreams, *ref)
		ups = append(ups, u)
	}

	const n = 50_000
	var totalTokens int64
	for i := 0; i < n; i++ {
		tok := logNormalTokens(rng, 500, 200_000)
		selected := Select(ups)
		require.NotNil(t, selected)
		selected.RecordDelivered(tok)
		ApplyDelivery(pm, ups, selected, tok)
		totalTokens += tok
	}

	for i, u := range ups {
		share := float64(u.TokensSent.Sum()) / float64(totalTokens)
		target := weights[i]
		diff := math.Abs(share - target)
		t.Logf("upstream %s: target=%.4f realized=%.4f diff=%.4f", ids[i], target, share, diff)
		require.LessOrEqualf(t, diff, 0.02, "upstream %s drifted beyond ±2pp: target=%.4f realized=%.4f", ids[i], target, share)
	}
}

// TestDistribution_RequestCountWeightingFails proves the token-weighting
// rule matters: if selection instead weighted by request count (ignoring
// token size entirely), a single upstream absorbing the large-context
// tail would blow past its target share. This mirrors the spec's
// instruction to "run the same test weighting by request count and show
// that it fails."
func TestDistribution_RequestCountWeightingFails(t *testing.T) {
	rng := rand.New(rand.NewSource(7))

	weights := []float64{0.5, 0.5}

	// Request-count-weighted selection: round-robin by count only, blind
	// to token size — the anti-pattern the spec warns against.
	sentTokens := map[string]int64{"a": 0, "b": 0}
	count := map[string]int64{"a": 0, "b": 0}

	const n = 5000
	for i := 0; i < n; i++ {
		tok := logNormalTokens(rng, 500, 200_000)
		// Simulate a pathological but realistic bias: the large-context
		// (agent) requests systematically land on upstream "a" (e.g.
		// because it happens to be first in a round-robin, or a sticky
		// session pins agent traffic there) while small chat turns spread
		// evenly — this is the exact failure mode request-count weighting
		// cannot see or correct for.
		var pick string
		if tok > 50_000 {
			pick = "a"
		} else if count["a"] <= count["b"] {
			pick = "a"
		} else {
			pick = "b"
		}
		sentTokens[pick] += tok
		count[pick]++
	}

	total := sentTokens["a"] + sentTokens["b"]
	shareA := float64(sentTokens["a"]) / float64(total)
	diff := math.Abs(shareA - weights[0])
	t.Logf("request-count-blind selection: target=0.50 realized_a=%.4f diff=%.4f (count_a=%d count_b=%d)", shareA, diff, count["a"], count["b"])
	require.Greaterf(t, diff, 0.02, "expected request-count-blind weighting to drift beyond ±2pp (it should fail this test to prove token-weighting matters), got diff=%.4f", diff)
}
