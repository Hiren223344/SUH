package proxy

import (
	"net/http"

	"router/internal/openai"
	"router/internal/sanitize"
)

// writeFatal writes a sanitized OpenAI-compatible error envelope. Used for
// every "propagate immediately, no retry" error: malformed body, unknown
// model, no capable upstream, client cancellation edge cases, and the
// terminal "all attempts exhausted" case.
func writeFatal(w http.ResponseWriter, status int, message string, typ openai.ErrorType) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(sanitize.Error(message, typ))
}
