package openai

import (
	"encoding/json"

	"router/internal/config"
	"router/internal/jsonutil"
)

// BuildUpstreamRequest clones the client's request body, rewrites it for a
// specific upstream (model name, per-upstream quirks), and clamps max
// output tokens to the upstream's ceiling. The client's original OrderedMap
// is never mutated, so the same request can be re-adapted for a different
// upstream on retry/fallback.
func BuildUpstreamRequest(clientReq *jsonutil.OrderedMap, up *config.Upstream) *jsonutil.OrderedMap {
	out := clientReq.Clone()

	_ = out.SetValue("model", up.Model)

	q := up.Quirks
	for _, field := range q.Strip {
		out.Delete(field)
	}
	if q.NoTemperature {
		out.Delete("temperature")
	}
	if q.NoTopP {
		out.Delete("top_p")
	}

	if q.MaxCompletionTokens {
		if raw, ok := out.Get("max_tokens"); ok {
			out.Set("max_completion_tokens", raw)
			out.Delete("max_tokens")
		}
	}
	clampMaxTokens(out, up.MaxOutput, q.MaxCompletionTokens)

	if q.StopAsString {
		normalizeStopToString(out)
	}

	return out
}

func clampMaxTokens(out *jsonutil.OrderedMap, ceiling int, useCompletionKey bool) {
	if ceiling <= 0 {
		return
	}
	key := "max_tokens"
	if useCompletionKey {
		key = "max_completion_tokens"
	}
	raw, ok := out.Get(key)
	if !ok {
		return
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return
	}
	if n > ceiling {
		_ = out.SetValue(key, ceiling)
	}
}

// normalizeStopToString collapses a single-element stop array to a bare
// string, for upstreams that reject the array form.
func normalizeStopToString(out *jsonutil.OrderedMap) {
	raw, ok := out.Get("stop")
	if !ok {
		return
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return // already a string, or unsupported shape — leave as-is
	}
	if len(arr) == 1 {
		_ = out.SetValue("stop", arr[0])
	} else if len(arr) == 0 {
		out.Delete("stop")
	}
}
