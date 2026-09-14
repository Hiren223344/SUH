package router

import (
	"testing"

	"github.com/stretchr/testify/require"

	"router/internal/config"
	"router/internal/openai"
	"router/internal/upstream"
)

func gateUpstream(id string, modalities []string, contextWindow int, tools, jsonSchema bool) *upstream.Upstream {
	cfg := &config.Upstream{
		ID: id, Modalities: modalities, ContextWindow: contextWindow,
		SupportsTools: tools, SupportsJSONSchema: jsonSchema, TPMLimit: 1 << 40,
	}
	return upstream.New(cfg, upstream.DefaultBreakerConfig())
}

// TestCapabilityGating_MultimodalNeverReachesTextOnly is acceptance test #5:
// a multimodal request must never reach a text-only upstream, at any
// weight, across 10,000 trials.
func TestCapabilityGating_MultimodalNeverReachesTextOnly(t *testing.T) {
	textOnly := gateUpstream("text-only", []string{"text"}, 128000, true, true)
	multimodal := gateUpstream("multimodal", []string{"text", "image"}, 128000, true, true)

	pm := &config.PublicModel{Upstreams: []config.PublicModelUpstream{
		{ID: "text-only", Weight: 0.99}, // heavily weighted, to prove weight can't override the gate
		{ID: "multimodal", Weight: 0.01},
	}}

	reg := &fakeRegistry{byID: map[string]*upstream.Upstream{"text-only": textOnly, "multimodal": multimodal}}

	in := GateInput{Signals: openai.Signals{HasImage: true}, EstimatedTotalTokens: 1000}
	for i := 0; i < 10_000; i++ {
		candidates := candidatesFrom(pm, reg, in)
		require.Len(t, candidates, 1)
		require.Equal(t, "multimodal", candidates[0].Cfg.ID)
		selected := Select(candidates)
		require.Equal(t, "multimodal", selected.Cfg.ID)
	}
}

// TestContextGating_LargeRequestOnlyReachesLargeWindow is acceptance test
// #6: a 190k-token request only reaches upstreams that can hold it.
func TestContextGating_LargeRequestOnlyReachesLargeWindow(t *testing.T) {
	small := gateUpstream("small", []string{"text"}, 32000, true, true)
	medium := gateUpstream("medium", []string{"text"}, 128000, true, true)
	large := gateUpstream("large", []string{"text"}, 200000, true, true)

	pm := &config.PublicModel{Upstreams: []config.PublicModelUpstream{
		{ID: "small", Weight: 0.33}, {ID: "medium", Weight: 0.33}, {ID: "large", Weight: 0.34},
	}}
	reg := &fakeRegistry{byID: map[string]*upstream.Upstream{"small": small, "medium": medium, "large": large}}

	in := GateInput{EstimatedTotalTokens: 190_000}
	for i := 0; i < 1000; i++ {
		candidates := candidatesFrom(pm, reg, in)
		for _, c := range candidates {
			require.Equal(t, "large", c.Cfg.ID, "a 190k-token request reached an upstream whose context window can't hold it")
		}
		require.NotEmpty(t, candidates)
	}
}

func TestContextGating_RespectsFivePercentMargin(t *testing.T) {
	// 128000 window * 0.95 = 121600. A 121601-token estimate must be gated
	// out; 121600 must pass.
	up := gateUpstream("u", []string{"text"}, 128000, true, true)
	pm := &config.PublicModel{Upstreams: []config.PublicModelUpstream{{ID: "u", Weight: 1}}}
	reg := &fakeRegistry{byID: map[string]*upstream.Upstream{"u": up}}

	tooBig := candidatesFrom(pm, reg, GateInput{EstimatedTotalTokens: 121601})
	require.Empty(t, tooBig)

	fits := candidatesFrom(pm, reg, GateInput{EstimatedTotalTokens: 121600})
	require.Len(t, fits, 1)
}

func TestToolGating(t *testing.T) {
	noTools := gateUpstream("no-tools", []string{"text"}, 128000, false, true)
	withTools := gateUpstream("with-tools", []string{"text"}, 128000, true, true)
	pm := &config.PublicModel{Upstreams: []config.PublicModelUpstream{
		{ID: "no-tools", Weight: 0.5}, {ID: "with-tools", Weight: 0.5},
	}}
	reg := &fakeRegistry{byID: map[string]*upstream.Upstream{"no-tools": noTools, "with-tools": withTools}}

	candidates := candidatesFrom(pm, reg, GateInput{Signals: openai.Signals{HasTools: true}})
	require.Len(t, candidates, 1)
	require.Equal(t, "with-tools", candidates[0].Cfg.ID)
}

func TestHealthGating_OpenBreakerExcluded(t *testing.T) {
	up := gateUpstream("u", []string{"text"}, 128000, true, true)
	for i := 0; i < 10; i++ {
		up.Breaker.RecordResult(false, nil)
	}
	require.Equal(t, upstream.StateOpen, up.Breaker.State())

	pm := &config.PublicModel{Upstreams: []config.PublicModelUpstream{{ID: "u", Weight: 1}}}
	reg := &fakeRegistry{byID: map[string]*upstream.Upstream{"u": up}}
	candidates := candidatesFrom(pm, reg, GateInput{})
	require.Empty(t, candidates)
}

// --- test doubles ---

type fakeRegistry struct{ byID map[string]*upstream.Upstream }

func candidatesFrom(pm *config.PublicModel, reg *fakeRegistry, in GateInput) []*upstream.Upstream {
	var out []*upstream.Upstream
	for _, ref := range pm.Upstreams {
		up, ok := reg.byID[ref.ID]
		if !ok || !passesGates(up, in) {
			continue
		}
		out = append(out, up)
	}
	return out
}
