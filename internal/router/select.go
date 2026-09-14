package router

import (
	"math/rand"

	"router/internal/config"
	"router/internal/upstream"
)

// jitterAmplitude bounds the random perturbation added to each candidate's
// debt score before comparison, so multiple router instances converging on
// the same drift signal don't all pick the identical upstream in lockstep.
const jitterAmplitude = 0.01

// Select implements Stage 2: token-weighted selection with drift
// correction. It picks the candidate with the highest accumulated debt
// (see upstream.Upstream.Debt), tie-broken by lowest current in-flight
// count.
func Select(candidates []*upstream.Upstream) *upstream.Upstream {
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	var best *upstream.Upstream
	var bestScore float64
	for _, c := range candidates {
		score := c.Debt.Load() + (rand.Float64()-0.5)*jitterAmplitude
		if best == nil || score > bestScore || (score == bestScore && c.InFlight.Load() < best.InFlight.Load()) {
			best = c
			bestScore = score
		}
	}
	return best
}

// ApplyDelivery updates the debt accumulator for every candidate that was
// eligible at selection time, after `winner` actually delivers
// tokensDelivered tokens to the client. Each candidate's target share is
// renormalized across exactly this candidate set (the "surviving candidate
// set" per the routing spec), so a candidate gated out of this particular
// request neither earns credit nor loses ground for it.
//
// Call this only with tokens that were actually delivered to the client —
// never for a hop that was tried and then failed over, per the "only
// tokens actually delivered count" rule.
func ApplyDelivery(pm *config.PublicModel, candidates []*upstream.Upstream, winner *upstream.Upstream, tokensDelivered int64) {
	if tokensDelivered <= 0 || len(candidates) == 0 {
		return
	}
	weights := weightMap(pm)
	var totalWeight float64
	for _, c := range candidates {
		totalWeight += weights[c.Cfg.ID]
	}
	if totalWeight <= 0 {
		return
	}
	t := float64(tokensDelivered)
	for _, c := range candidates {
		share := weights[c.Cfg.ID] / totalWeight
		c.Debt.Add(share * t)
	}
	winner.Debt.Add(-t)
}

func weightMap(pm *config.PublicModel) map[string]float64 {
	m := make(map[string]float64, len(pm.Upstreams))
	for _, u := range pm.Upstreams {
		m[u.ID] = u.Weight
	}
	return m
}
