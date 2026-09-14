package openai

import (
	"testing"

	"github.com/stretchr/testify/require"

	"router/internal/config"
)

func TestBuildUpstreamRequest_SetsModelAndStripsQuirkFields(t *testing.T) {
	req := parseReq(t, `{"model":"M","messages":[],"logit_bias":{"1":1},"seed":42,"temperature":0.7}`)
	up := &config.Upstream{
		Model:  "backend-model-x",
		Quirks: config.Quirks{Strip: []string{"logit_bias", "seed"}},
	}
	out := BuildUpstreamRequest(req, up)

	model, ok := out.GetString("model")
	require.True(t, ok)
	require.Equal(t, "backend-model-x", model)
	require.False(t, out.Has("logit_bias"))
	require.False(t, out.Has("seed"))
	require.True(t, out.Has("temperature"))

	// The original client request must be untouched (so it can be re-adapted
	// for a different upstream on retry).
	require.True(t, req.Has("logit_bias"))
	origModel, _ := req.GetString("model")
	require.Equal(t, "M", origModel)
}

func TestBuildUpstreamRequest_NoTemperatureNoTopP(t *testing.T) {
	req := parseReq(t, `{"model":"M","messages":[],"temperature":0.5,"top_p":0.9}`)
	up := &config.Upstream{Model: "m", Quirks: config.Quirks{NoTemperature: true, NoTopP: true}}
	out := BuildUpstreamRequest(req, up)
	require.False(t, out.Has("temperature"))
	require.False(t, out.Has("top_p"))
}

func TestBuildUpstreamRequest_RenamesMaxTokensToMaxCompletionTokens(t *testing.T) {
	req := parseReq(t, `{"model":"M","messages":[],"max_tokens":500}`)
	up := &config.Upstream{Model: "m", MaxOutput: 8192, Quirks: config.Quirks{MaxCompletionTokens: true}}
	out := BuildUpstreamRequest(req, up)
	require.False(t, out.Has("max_tokens"))
	raw, ok := out.Get("max_completion_tokens")
	require.True(t, ok)
	require.JSONEq(t, "500", string(raw))
}

func TestBuildUpstreamRequest_ClampsMaxTokensToCeiling(t *testing.T) {
	req := parseReq(t, `{"model":"M","messages":[],"max_tokens":100000}`)
	up := &config.Upstream{Model: "m", MaxOutput: 4096}
	out := BuildUpstreamRequest(req, up)
	raw, ok := out.Get("max_tokens")
	require.True(t, ok)
	require.JSONEq(t, "4096", string(raw))
}

func TestBuildUpstreamRequest_StopAsStringCollapsesSingleElementArray(t *testing.T) {
	req := parseReq(t, `{"model":"M","messages":[],"stop":["END"]}`)
	up := &config.Upstream{Model: "m", Quirks: config.Quirks{StopAsString: true}}
	out := BuildUpstreamRequest(req, up)
	raw, ok := out.Get("stop")
	require.True(t, ok)
	require.JSONEq(t, `"END"`, string(raw))
}

func TestBuildUpstreamRequest_StopAsStringDropsEmptyArray(t *testing.T) {
	req := parseReq(t, `{"model":"M","messages":[],"stop":[]}`)
	up := &config.Upstream{Model: "m", Quirks: config.Quirks{StopAsString: true}}
	out := BuildUpstreamRequest(req, up)
	require.False(t, out.Has("stop"))
}

func TestBuildUpstreamRequest_MultiElementStopArrayLeftAlone(t *testing.T) {
	req := parseReq(t, `{"model":"M","messages":[],"stop":["A","B"]}`)
	up := &config.Upstream{Model: "m", Quirks: config.Quirks{StopAsString: true}}
	out := BuildUpstreamRequest(req, up)
	raw, ok := out.Get("stop")
	require.True(t, ok)
	require.JSONEq(t, `["A","B"]`, string(raw))
}
