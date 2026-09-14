// Package sanitize is the single chokepoint every client-bound byte must
// pass through. Its job is identity opacity: the client asked for public
// model M, and must never see anything that reveals which upstream actually
// served the request — not in .model, not in .id, not in headers, and
// especially not in error bodies.
package sanitize

import (
	"encoding/json"

	"router/internal/jsonutil"
	"router/internal/openai"
)

// usageAllowlist is the set of OpenAI-standard usage sub-fields we permit
// through. Anything else (provider-specific cost/billing/cache metadata)
// is dropped because it can fingerprint the backend.
var usageAllowlist = map[string]bool{
	"prompt_tokens":             true,
	"completion_tokens":         true,
	"total_tokens":              true,
	"prompt_tokens_details":     true,
	"completion_tokens_details": true,
}

// topLevelAllowlist is the set of top-level response fields OpenAI's own
// schema defines. Anything else an upstream adds (e.g. a provider trace id,
// a billing field) is stripped.
//
// system_fingerprint is deliberately NOT included even though it's a real
// OpenAI schema field: in practice providers encode backend/build identity
// into its value (e.g. "fp_qwen3.8-flash"), which is exactly the kind of
// leak identity opacity forbids. Dropping the field entirely is cheaper and
// safer than trying to sanitize its content.
var topLevelAllowlist = map[string]bool{
	"id":           true,
	"object":       true,
	"created":      true,
	"model":        true,
	"choices":      true,
	"usage":        true,
	"service_tier": true,
}

// Object sanitizes one JSON object — a full non-streaming response or a
// single SSE chunk — in place and returns it re-marshaled. publicModel is
// what the client asked for; requestID is the router's own request id,
// substituted for whatever the upstream returned.
func Object(raw []byte, publicModel, requestID string) ([]byte, error) {
	m, err := jsonutil.ParseObject(raw)
	if err != nil {
		return nil, err
	}
	Rewrite(m, publicModel, requestID)
	return m.MarshalJSON()
}

// Rewrite applies the identity-opacity transform to an already-parsed
// response/chunk object: pin .model to the public name, replace .id with
// our own request id, strip non-schema top-level fields, and scrub usage
// sub-fields down to the standard allowlist.
func Rewrite(m *jsonutil.OrderedMap, publicModel, requestID string) {
	if m.Has("model") {
		_ = m.SetValue("model", publicModel)
	}
	if m.Has("id") {
		_ = m.SetValue("id", requestID)
	}
	m.KeepOnly(topLevelAllowlist)

	if usageRaw, ok := m.Get("usage"); ok {
		if usageMap, err := jsonutil.ParseObject(usageRaw); err == nil {
			usageMap.KeepOnly(usageAllowlist)
			if b, err := usageMap.MarshalJSON(); err == nil {
				m.Set("usage", b)
			}
		}
	}
}

// Error builds a client-facing error envelope that is guaranteed not to
// contain any upstream-originated text, header, or identifier. Callers must
// never forward an upstream error body directly — always go through this.
func Error(message string, typ openai.ErrorType) []byte {
	return openai.NewError(message, typ, nil, nil).JSON()
}

// StreamTerminalError renders a well-formed terminal SSE error chunk
// followed by "[DONE]", so a client mid-stream sees a clean close instead
// of a hang. Used only after the first content chunk has already been
// flushed and the stream is committed.
func StreamTerminalError(message string, typ openai.ErrorType) []byte {
	body := openai.NewError(message, typ, nil, nil)
	b, _ := json.Marshal(body)
	out := append([]byte("data: "), b...)
	out = append(out, []byte("\n\ndata: [DONE]\n\n")...)
	return out
}
