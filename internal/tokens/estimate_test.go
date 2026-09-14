package tokens

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"router/internal/openai"
)

func msg(role, content string) openai.ChatMessage {
	b, _ := json.Marshal(content)
	return openai.ChatMessage{Role: role, Content: b}
}

func TestEstimatePromptTokens_PlainText(t *testing.T) {
	msgs := []openai.ChatMessage{
		msg("system", "You are a helpful assistant."),
		msg("user", "Hello there, how are you today?"),
	}
	n := EstimatePromptTokens(msgs)
	require.Greater(t, n, 0)
	require.Less(t, n, 100) // sanity bound for ~65 chars of text
}

func TestEstimatePromptTokens_MultiPartContentWithImage(t *testing.T) {
	parts := []map[string]string{
		{"type": "text", "text": "what is in this image?"},
		{"type": "image_url"},
	}
	raw, _ := json.Marshal(parts)
	msgs := []openai.ChatMessage{{Role: "user", Content: raw}}

	textOnly := EstimatePromptTokens([]openai.ChatMessage{msg("user", "what is in this image?")})
	withImage := EstimatePromptTokens(msgs)
	require.Greater(t, withImage, textOnly+700, "an image part should add a large flat token cost")
}

func TestEstimateTotal_UsesMaxTokensWhenSet(t *testing.T) {
	s := openai.Signals{
		Messages:  []openai.ChatMessage{msg("user", "hi")},
		MaxTokens: 2000,
	}
	prompt, completion, total := EstimateTotal(s)
	require.Equal(t, 2000, completion)
	require.Equal(t, prompt+completion, total)
}

func TestEstimateTotal_DefaultsCompletionWhenUnset(t *testing.T) {
	s := openai.Signals{Messages: []openai.ChatMessage{msg("user", "hi")}}
	_, completion, _ := EstimateTotal(s)
	require.Equal(t, defaultCompletionEstimate, completion)
}

func TestParseUsage_StandardFields(t *testing.T) {
	raw := json.RawMessage(`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`)
	u, ok := ParseUsage(raw)
	require.True(t, ok)
	require.Equal(t, Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, u)
}

func TestParseUsage_ComputesTotalWhenMissing(t *testing.T) {
	raw := json.RawMessage(`{"prompt_tokens":10,"completion_tokens":5}`)
	u, ok := ParseUsage(raw)
	require.True(t, ok)
	require.Equal(t, 15, u.TotalTokens)
}

func TestParseUsage_EmptyIsNotOK(t *testing.T) {
	_, ok := ParseUsage(nil)
	require.False(t, ok)
}
