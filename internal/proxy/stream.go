package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"router/internal/jsonutil"
	"router/internal/openai"
	"router/internal/sanitize"
	"router/internal/stats"
	"router/internal/tokens"
	"router/internal/upstream"
)

// sseReader extracts successive "data:" payloads from an SSE byte stream,
// ignoring other SSE fields (event:, id:, retry:, comments, blank lines).
type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(body *bufio.Reader) *sseReader {
	return &sseReader{r: body}
}

func (s *sseReader) Next() (string, error) {
	for {
		line, err := s.r.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimPrefix(trimmed, "data:")
			payload = strings.TrimPrefix(payload, " ")
			if payload != "" {
				return payload, nil
			}
		}
		if err != nil {
			return "", err
		}
	}
}

// serveStream implements the streaming retry window: it withholds every
// byte from the client until the upstream's response headers AND first
// SSE data event have both arrived. Only then is anything committed to the
// client socket. Before that point, any failure is safely retryable
// against another upstream (return committed=false). After that point, a
// failure can only be terminated cleanly — a well-formed terminal error
// chunk plus [DONE] — because the client has already committed to this
// stream. The third return value is the number of tokens actually
// delivered to the client (from this stream's own usage chunk, when
// stream_options.include_usage was requested) — never derived by diffing
// the upstream's shared TokensSent window, which would race against other
// concurrent requests to the same upstream.
func serveStream(ctx context.Context, w http.ResponseWriter, up *upstream.Upstream, clientReq *jsonutil.OrderedMap, publicModel, reqID string, rec *stats.Recorder, logger *slog.Logger) (result stepResult, committed bool, delivered int64) {
	start := time.Now()
	resp, err := dispatch(ctx, up, clientReq)
	if err != nil {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		if ctx.Err() != nil {
			return stepClientGone, false, 0
		}
		logger.Warn("upstream dispatch failed", "upstream", up.Cfg.ID, "error", err)
		return stepRetry, false, 0
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		logger.Warn("upstream returned error status", "upstream", up.Cfg.ID, "status", resp.StatusCode)
		return stepRetry, false, 0
	}

	sse := newSSEReader(bufio.NewReaderSize(resp.Body, 64*1024))

	// Pre-commit: pull the first data event without writing anything.
	firstPayload, err := sse.Next()
	if err != nil {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		if ctx.Err() != nil {
			return stepClientGone, false, 0
		}
		logger.Warn("upstream stream failed before first chunk", "upstream", up.Cfg.ID, "error", err)
		return stepRetry, false, 0
	}
	rec.RecordTTFT(up.Cfg.ID, time.Since(start))

	// Committed from here on: write headers, then the buffered first chunk.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var deliveredTokens int64
	writeEvent := func(payload string) bool {
		if payload == "[DONE]" {
			_, werr := w.Write([]byte("data: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return werr == nil
		}
		obj, perr := jsonutil.ParseObject([]byte(payload))
		if perr != nil {
			// Malformed upstream chunk: skip rather than forward garbage or
			// leak the raw upstream bytes to the client.
			return true
		}
		if usageRaw, ok := obj.Get("usage"); ok {
			if u, ok := tokens.ParseUsage(usageRaw); ok {
				deliveredTokens = int64(u.TotalTokens)
			}
		}
		sanitize.Rewrite(obj, publicModel, reqID)
		b, merr := obj.MarshalJSON()
		if merr != nil {
			return true
		}
		if _, werr := w.Write([]byte("data: ")); werr != nil {
			return false
		}
		if _, werr := w.Write(b); werr != nil {
			return false
		}
		if _, werr := w.Write([]byte("\n\n")); werr != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	if !writeEvent(firstPayload) {
		// Client vanished exactly as we flushed the first byte. Nothing
		// more we can or should do.
		up.Breaker.RecordResult(true, func() { up.Debt.Store(0) })
		rec.RecordOutcome(up.Cfg.ID, true)
		return stepClientGone, true, 0
	}

	for {
		payload, err := sse.Next()
		if err != nil {
			// sseReader.Next never pairs a non-empty payload with a non-nil
			// error (a trailing unterminated "data:" line is returned whole,
			// with a nil error, on the call that reads it; the reader only
			// surfaces EOF/read errors once nothing further remains) — so
			// there is no pending payload to flush here.
			if isCleanEOF(err) {
				break
			}
			// Mid-stream failure after commit: cannot fail over. Terminate
			// the stream cleanly and log loudly — this is the one failure
			// mode this design cannot hide from the client.
			up.Breaker.RecordResult(false, nil)
			rec.RecordOutcome(up.Cfg.ID, false)
			logger.Error("committed stream failed mid-flight", "upstream", up.Cfg.ID, "upstream_model", up.Cfg.Model, "request_id", reqID, "error", err)
			_, _ = w.Write(sanitize.StreamTerminalError("upstream connection lost", openai.ErrTypeAPIError))
			if flusher != nil {
				flusher.Flush()
			}
			if deliveredTokens > 0 {
				up.RecordDelivered(deliveredTokens)
			}
			return stepSuccess, true, deliveredTokens // committed: caller must not retry
		}
		if payload == "[DONE]" {
			writeEvent(payload)
			break
		}
		if !writeEvent(payload) {
			// Client disconnected mid-stream; upstream request context will
			// be cancelled by the caller via ctx propagation.
			up.Breaker.RecordResult(true, func() { up.Debt.Store(0) })
			rec.RecordOutcome(up.Cfg.ID, true)
			if deliveredTokens > 0 {
				up.RecordDelivered(deliveredTokens)
			}
			return stepClientGone, true, deliveredTokens
		}
	}

	up.Breaker.RecordResult(true, func() { up.Debt.Store(0) })
	rec.RecordOutcome(up.Cfg.ID, true)
	if deliveredTokens > 0 {
		up.RecordDelivered(deliveredTokens)
	}
	return stepSuccess, true, deliveredTokens
}

func isCleanEOF(err error) bool {
	return errors.Is(err, io.EOF)
}
