package router

import (
	"router/internal/config"
	"router/internal/openai"
	"router/internal/upstream"
)

// GateInput carries the per-request facts Stage 1 gates check against.
type GateInput struct {
	Signals              openai.Signals
	EstimatedTotalTokens int64 // prompt + expected completion, for context-window and TPM checks
}

// Candidates returns the upstreams configured for public model pm that
// survive every Stage 1 hard gate, excluding any id in exclude (already
// tried on a prior fallback hop for this request).
func Candidates(pm *config.PublicModel, reg *Registry, in GateInput, exclude map[string]bool) []*upstream.Upstream {
	var out []*upstream.Upstream
	for _, ref := range pm.Upstreams {
		if exclude[ref.ID] {
			continue
		}
		up := reg.Get(ref.ID)
		if up == nil {
			continue
		}
		if !passesGates(up, in) {
			continue
		}
		out = append(out, up)
	}
	return out
}

// FallbackCandidates returns only the upstreams marked as the universal
// fallback pool for pm, still subject to Stage 1 gating. Used when the
// primary candidate set is empty.
func FallbackCandidates(pm *config.PublicModel, reg *Registry, in GateInput, exclude map[string]bool) []*upstream.Upstream {
	var out []*upstream.Upstream
	for _, ref := range pm.Upstreams {
		if !ref.Fallback || exclude[ref.ID] {
			continue
		}
		up := reg.Get(ref.ID)
		if up == nil {
			continue
		}
		if !passesGates(up, in) {
			continue
		}
		out = append(out, up)
	}
	return out
}

func passesGates(up *upstream.Upstream, in GateInput) bool {
	cfg := up.Cfg

	if in.Signals.HasImage && !hasModality(cfg.Modalities, "image") {
		return false
	}
	if in.Signals.HasAudio && !hasModality(cfg.Modalities, "audio") {
		return false
	}
	if in.Signals.HasTools && !cfg.SupportsTools {
		return false
	}
	if (in.Signals.ResponseFormat == "json_object" || in.Signals.ResponseFormat == "json_schema") && !cfg.SupportsJSONSchema {
		return false
	}
	if cfg.ContextWindow > 0 {
		margin := int64(float64(cfg.ContextWindow) * 0.95) // 5% safety margin
		if in.EstimatedTotalTokens > margin {
			return false
		}
	}
	if up.Breaker.State() == upstream.StateOpen {
		return false
	}
	if !up.HasTPMCapacity(in.EstimatedTotalTokens) {
		return false
	}
	return true
}

func hasModality(modalities []string, want string) bool {
	for _, m := range modalities {
		if m == want {
			return true
		}
	}
	return false
}
