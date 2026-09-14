// Package tokens provides pre-flight token cost estimation (used for
// capability gating, weighted selection, and TPM budgeting before an
// upstream call is made) and post-response reconciliation against actual
// usage figures.
package tokens

import (
	"encoding/json"
	"unicode/utf8"

	"router/internal/openai"
)

// charsPerToken is a rough, deliberately conservative estimate (OpenAI's
// own rule of thumb is ~4 chars/token for English text). It only needs to
// be good enough for gating and initial weighted selection — the estimate
// is always reconciled against real usage once the response completes.
const charsPerToken = 4

// imageTokenEstimate is a flat per-image-part cost, roughly matching a
// mid-resolution "auto" detail image in OpenAI's own accounting.
const imageTokenEstimate = 765

// audioTokenEstimate is a flat per-audio-part cost placeholder; providers
// vary widely and audio duration isn't available pre-flight.
const audioTokenEstimate = 1000

// defaultCompletionEstimate is used when a request sets no max_tokens /
// max_completion_tokens, so we still have a non-zero completion estimate
// for budgeting purposes.
const defaultCompletionEstimate = 1024

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// EstimatePromptTokens walks the request's messages and returns a rough
// prompt token count.
func EstimatePromptTokens(msgs []openai.ChatMessage) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Role)/charsPerToken + 1 // role + message overhead
		total += estimateContentTokens(m.Content)
		if len(m.ToolCalls) > 0 {
			total += utf8.RuneCount(m.ToolCalls) / charsPerToken
		}
	}
	return total
}

func estimateContentTokens(content json.RawMessage) int {
	if len(content) == 0 {
		return 0
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return utf8.RuneCountInString(s)/charsPerToken + 1
	}
	var parts []contentPart
	if err := json.Unmarshal(content, &parts); err == nil {
		total := 0
		for _, p := range parts {
			switch p.Type {
			case "text":
				total += utf8.RuneCountInString(p.Text)/charsPerToken + 1
			case "image_url":
				total += imageTokenEstimate
			case "input_audio":
				total += audioTokenEstimate
			default:
				total += 16
			}
		}
		return total
	}
	return utf8.RuneCount(content) / charsPerToken
}

// EstimateTotal returns the pre-flight estimated total cost (prompt +
// expected completion) used for gating a request against context windows
// and TPM budgets.
func EstimateTotal(s openai.Signals) (promptTokens, completionTokens, total int) {
	promptTokens = EstimatePromptTokens(s.Messages)
	completionTokens = s.MaxTokens
	if completionTokens <= 0 {
		completionTokens = defaultCompletionEstimate
	}
	return promptTokens, completionTokens, promptTokens + completionTokens
}

// Usage is the reconciled actual token cost of a completed request.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ParseUsage extracts prompt/completion/total token counts from an
// upstream's usage object (already scrubbed by sanitize, or raw — this
// only reads the three standard integer fields).
func ParseUsage(raw json.RawMessage) (Usage, bool) {
	if len(raw) == 0 {
		return Usage{}, false
	}
	var u struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return Usage{}, false
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}, true
}
