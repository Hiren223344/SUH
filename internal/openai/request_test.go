package openai

import (
	"testing"

	"github.com/stretchr/testify/require"

	"router/internal/jsonutil"
)

func parseReq(t *testing.T, raw string) *jsonutil.OrderedMap {
	t.Helper()
	m, err := jsonutil.ParseObject([]byte(raw))
	require.NoError(t, err)
	return m
}

func TestExtractSignals_PlainTextRequest(t *testing.T) {
	m := parseReq(t, `{"model":"M","messages":[{"role":"user","content":"hi"}]}`)
	s, err := ExtractSignals(m)
	require.NoError(t, err)
	require.Equal(t, "M", s.Model)
	require.False(t, s.HasImage)
	require.False(t, s.HasAudio)
	require.False(t, s.HasTools)
	require.Empty(t, s.ResponseFormat)
}

func TestExtractSignals_DetectsImagePart(t *testing.T) {
	m := parseReq(t, `{"model":"M","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"http://x"}}]}]}`)
	s, err := ExtractSignals(m)
	require.NoError(t, err)
	require.True(t, s.HasImage)
	require.False(t, s.HasAudio)
}

func TestExtractSignals_DetectsAudioPart(t *testing.T) {
	m := parseReq(t, `{"model":"M","messages":[{"role":"user","content":[{"type":"input_audio"}]}]}`)
	s, err := ExtractSignals(m)
	require.NoError(t, err)
	require.True(t, s.HasAudio)
}

func TestExtractSignals_ToolsFromToolsField(t *testing.T) {
	m := parseReq(t, `{"model":"M","messages":[],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	s, err := ExtractSignals(m)
	require.NoError(t, err)
	require.True(t, s.HasTools)
}

func TestExtractSignals_ToolsFromHistory(t *testing.T) {
	m := parseReq(t, `{"model":"M","messages":[{"role":"tool","content":"result"}]}`)
	s, err := ExtractSignals(m)
	require.NoError(t, err)
	require.True(t, s.HasTools)
}

func TestExtractSignals_ToolsFromAssistantToolCalls(t *testing.T) {
	m := parseReq(t, `{"model":"M","messages":[{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`)
	s, err := ExtractSignals(m)
	require.NoError(t, err)
	require.True(t, s.HasTools)
}

func TestExtractSignals_ResponseFormatAndStreamOptions(t *testing.T) {
	m := parseReq(t, `{"model":"M","messages":[],"stream":true,"stream_options":{"include_usage":true},"response_format":{"type":"json_schema"},"max_tokens":500}`)
	s, err := ExtractSignals(m)
	require.NoError(t, err)
	require.True(t, s.Stream)
	require.True(t, s.IncludeUsage)
	require.Equal(t, "json_schema", s.ResponseFormat)
	require.Equal(t, 500, s.MaxTokens)
}

func TestExtractSignals_MaxCompletionTokensPreferredIfLarger(t *testing.T) {
	m := parseReq(t, `{"model":"M","messages":[],"max_tokens":100,"max_completion_tokens":900}`)
	s, err := ExtractSignals(m)
	require.NoError(t, err)
	require.Equal(t, 900, s.MaxTokens)
}
