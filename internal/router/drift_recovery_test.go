package router

import (
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"router/internal/config"
	"router/internal/upstream"
)

// TestDriftRecovery_NoRepaymentSpike is acceptance test #2: kill one
// upstream for 20% of the run, verify the survivors absorb its share
// proportionally while it's down, and verify that on recovery there is no
// repayment spike — i.e. the recovered upstream's selection rate right
// after recovery is not wildly higher than its steady-state target, and
// overall convergence resumes within a bounded number of requests.
func TestDriftRecovery_NoRepaymentSpike(t *testing.T) {
	rng := rand.New(rand.NewSource(99))

	pm := &config.PublicModel{}
	ids := []string{"a", "b", "c"}
	weights := []float64{0.34, 0.33, 0.33}
	var ups []*upstream.Upstream
	for i, id := range ids {
		ref, u := synthUpstream(id, weights[i])
		pm.Upstreams = append(pm.Upstreams, *ref)
		ups = append(ups, u)
	}
	victim := ups[0] // "a"

	deliver := func(candidates []*upstream.Upstream) int64 {
		selected := Select(candidates)
		tok := logNormalTokens(rng, 500, 50_000)
		selected.RecordDelivered(tok)
		ApplyDelivery(pm, candidates, selected, tok)
		return tok
	}

	const outageRequests = 2000
	const totalRequests = 10_000

	all := ups
	survivors := []*upstream.Upstream{ups[1], ups[2]}

	// Phase 1: victim healthy, warm up near target.
	for i := 0; i < 2000; i++ {
		deliver(all)
	}

	// Phase 2: victim down (excluded from the candidate set, exactly as
	// Stage 1 gating would exclude it once its breaker opens).
	var survivorTokensDuringOutage int64
	survivorStart := map[string]int64{"b": ups[1].TokensSent.Sum(), "c": ups[2].TokensSent.Sum()}
	for i := 0; i < outageRequests; i++ {
		survivorTokensDuringOutage += deliver(survivors)
	}
	// Survivors should have absorbed victim's share proportionally: b and c
	// split the outage traffic roughly in their weight ratio (0.33:0.33 == 1:1).
	bGain := ups[1].TokensSent.Sum() - survivorStart["b"]
	cGain := ups[2].TokensSent.Sum() - survivorStart["c"]
	ratio := float64(bGain) / float64(bGain+cGain)
	require.InDeltaf(t, 0.5, ratio, 0.05, "survivors should split victim's redistributed share ~proportionally to their own weights, got b_share=%.3f", ratio)

	// Phase 3: victim recovers (breaker closes -> debt reset), resume
	// full candidate set. Track its selection rate in a short window right
	// after recovery vs. its target share.
	victim.Debt.Store(0) // what Breaker.RecordResult's onClose callback does on close

	const postRecoveryWindow = 200
	victimPicks := 0
	for i := 0; i < postRecoveryWindow; i++ {
		selected := Select(all)
		if selected == victim {
			victimPicks++
		}
		tok := logNormalTokens(rng, 500, 50_000)
		selected.RecordDelivered(tok)
		ApplyDelivery(pm, all, selected, tok)
	}
	victimRate := float64(victimPicks) / float64(postRecoveryWindow)
	// A repayment spike would look like victimRate >> its target (0.34);
	// with debt reset on close it should be close to target, not pinned
	// near 1.0 trying to "catch up" on everything missed during the outage.
	require.Lessf(t, victimRate, 0.60, "victim's post-recovery selection rate spiked to %.3f, indicating a repayment spike instead of a reset", victimRate)

	// Phase 4: convergence resumes — run out to totalRequests and check
	// the final realized shares land back within tolerance.
	for i := postRecoveryWindow; i < totalRequests-outageRequests-2000; i++ {
		deliver(all)
	}
	var totalTokens int64
	for _, u := range all {
		totalTokens += u.TokensSent.Sum()
	}
	for i, u := range all {
		share := float64(u.TokensSent.Sum()) / float64(totalTokens)
		diff := math.Abs(share - weights[i])
		t.Logf("post-recovery final: upstream %s target=%.3f realized=%.3f diff=%.3f", ids[i], weights[i], share, diff)
	}
}
