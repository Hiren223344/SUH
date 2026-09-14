package openai

import (
	"encoding/json"

	"router/internal/jsonutil"
)

// ChatMessage is the minimal shape we need to inspect a message for routing
// decisions (modality, tool history) and token estimation. Unknown fields
// are preserved in the original request body untouched — this struct is
// read-only tooling, never re-serialized back to clients or upstreams.
type ChatMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls"`
}

// contentPart mirrors an OpenAI multi-part message content entry.
type contentPart struct {
	Type string `json:"type"`
}

// Signals summarizes the routing-relevant facts about a chat completion
// request, extracted once and passed to gating/estimation/logging.
type Signals struct {
	Model            string
	Stream           bool
	IncludeUsage     bool
	HasImage         bool
	HasAudio         bool
	HasTools         bool // tools/tool_choice present, or history contains tool_calls/role:tool
	ResponseFormat   string // "", "json_object", or "json_schema"
	MaxTokens        int // from max_tokens or max_completion_tokens; 0 if unset
	Messages         []ChatMessage
}

// Messages unmarshals the request's "messages" array.
func Messages(req *jsonutil.OrderedMap) ([]ChatMessage, error) {
	raw, ok := req.Get("messages")
	if !ok {
		return nil, nil
	}
	var msgs []ChatMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// messageHasModality reports whether a message's content parts include the
// given OpenAI content-part type prefix (e.g. "image_url", "input_audio").
func messageHasPartType(content json.RawMessage, partType string) bool {
	if len(content) == 0 {
		return false
	}
	var parts []contentPart
	if err := json.Unmarshal(content, &parts); err != nil {
		return false // content was a plain string, not a parts array
	}
	for _, p := range parts {
		if p.Type == partType {
			return true
		}
	}
	return false
}

// ExtractSignals inspects a chat completion request body and returns the
// facts Stage 1 gating, token estimation, and logging need.
func ExtractSignals(req *jsonutil.OrderedMap) (Signals, error) {
	var s Signals

	s.Model, _ = req.GetString("model")

	if raw, ok := req.Get("stream"); ok {
		_ = json.Unmarshal(raw, &s.Stream)
	}
	if raw, ok := req.Get("stream_options"); ok {
		var opts struct {
			IncludeUsage bool `json:"include_usage"`
		}
		if json.Unmarshal(raw, &opts) == nil {
			s.IncludeUsage = opts.IncludeUsage
		}
	}

	if req.Has("tools") || req.Has("tool_choice") {
		s.HasTools = true
	}

	msgs, err := Messages(req)
	if err != nil {
		return s, err
	}
	s.Messages = msgs
	for _, m := range msgs {
		if messageHasPartType(m.Content, "image_url") {
			s.HasImage = true
		}
		if messageHasPartType(m.Content, "input_audio") {
			s.HasAudio = true
		}
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			s.HasTools = true
		}
	}

	if raw, ok := req.Get("response_format"); ok {
		var rf struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &rf) == nil {
			s.ResponseFormat = rf.Type
		}
	}

	if raw, ok := req.Get("max_tokens"); ok {
		var n int
		if json.Unmarshal(raw, &n) == nil {
			s.MaxTokens = n
		}
	}
	if raw, ok := req.Get("max_completion_tokens"); ok {
		var n int
		if json.Unmarshal(raw, &n) == nil && n > s.MaxTokens {
			s.MaxTokens = n
		}
	}

	return s, nil
}
