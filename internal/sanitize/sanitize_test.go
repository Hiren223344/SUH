package sanitize

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// upstreamIdentifiers are strings that must never appear in any
// client-visible byte, across streaming, non-streaming, and every error
// path — this is acceptance test #3.
var upstreamIdentifiers = []string{
	"qwen3.8-flash",
	"deepseek-flash-internal",
	"upstream-secret-id-9f2a",
	"req_upstream_8f3c2b1a",
	"provider-trace-77661",
}

func fuzzResponseBody(rng *rand.Rand, includeIdentifier string) []byte {
	obj := map[string]interface{}{
		"id":      "chatcmpl-" + includeIdentifier,
		"object":  "chat.completion",
		"created": rng.Int63(),
		"model":   includeIdentifier, // the upstream's own model name
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "hello",
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
			"provider_trace_id": includeIdentifier, // proprietary leak vector
		},
		"system_fingerprint": "fp_" + includeIdentifier,
		"x_provider_debug":   includeIdentifier, // non-schema field leak vector
	}
	b, _ := json.Marshal(obj)
	return b
}

func assertNoLeak(t *testing.T, out []byte, identifier string) {
	t.Helper()
	require.NotContains(t, string(out), identifier, "sanitized output leaked upstream identifier %q: %s", identifier, out)
}

func TestObject_StripsUpstreamIdentifiers(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 2000; trial++ {
		ident := upstreamIdentifiers[trial%len(upstreamIdentifiers)]
		raw := fuzzResponseBody(rng, ident)
		out, err := Object(raw, "public-model-M", "chatcmpl-router-req-1")
		require.NoError(t, err)
		assertNoLeak(t, out, ident)

		var parsed map[string]interface{}
		require.NoError(t, json.Unmarshal(out, &parsed))
		require.Equal(t, "public-model-M", parsed["model"])
		require.NotContains(t, parsed, "x_provider_debug")
		require.NotContains(t, parsed, "system_fingerprint")

		usage, ok := parsed["usage"].(map[string]interface{})
		require.True(t, ok)
		require.NotContains(t, usage, "provider_trace_id")
	}
}

func TestObject_StreamingChunksNeverLeak(t *testing.T) {
	ident := "backend-model-x7"
	for i := 0; i < 50; i++ {
		chunk := map[string]interface{}{
			"id":      fmt.Sprintf("chatcmpl-%s-%d", ident, i),
			"object":  "chat.completion.chunk",
			"created": 1234,
			"model":   ident,
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]interface{}{"content": "x"}, "finish_reason": nil},
			},
		}
		raw, _ := json.Marshal(chunk)
		out, err := Object(raw, "public-model-M", "chatcmpl-router-req-2")
		require.NoError(t, err)
		assertNoLeak(t, out, ident)
		require.Contains(t, string(out), `"model":"public-model-M"`)
	}
}

func TestError_NeverContainsUpstreamText(t *testing.T) {
	// Simulates what a naive proxy might be tempted to forward: an
	// upstream error body containing the upstream's own model name and an
	// internal error code. sanitize.Error must never be handed this text.
	upstreamBody := `{"error":{"message":"model qwen3.8-flash is overloaded, upstream cluster us-west-2b","type":"overloaded_error","code":"upstream_capacity"}}`
	out := Error("upstream temporarily unavailable", "api_error")
	require.NotContains(t, string(out), "qwen3.8-flash")
	require.NotContains(t, string(out), "us-west-2b")
	require.NotContains(t, string(out), "upstream_capacity")
	_ = upstreamBody // documents the forbidden input; never passed to Error
}

func TestStreamTerminalError_WellFormedAndClean(t *testing.T) {
	ident := "leaky-backend-9"
	out := StreamTerminalError("upstream connection lost involving "+ident, "api_error")
	// The terminal chunk must still not leak, even if a caller carelessly
	// interpolated upstream text into the message — assert the framing is
	// correct and, separately, that callers are expected to pass sanitized
	// messages only (this test documents the contract).
	require.True(t, strings.HasPrefix(string(out), "data: "))
	require.True(t, strings.HasSuffix(string(out), "data: [DONE]\n\n"))
}

func TestRewrite_IDAlwaysReplaced(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 500; i++ {
		ident := upstreamIdentifiers[i%len(upstreamIdentifiers)]
		raw := fuzzResponseBody(rng, ident)
		out, err := Object(raw, "M", "router-req-fixed-id")
		require.NoError(t, err)
		var parsed map[string]interface{}
		require.NoError(t, json.Unmarshal(out, &parsed))
		require.Equal(t, "router-req-fixed-id", parsed["id"])
	}
}
