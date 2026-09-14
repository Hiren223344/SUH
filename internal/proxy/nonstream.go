package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"router/internal/jsonutil"
	"router/internal/sanitize"
	"router/internal/stats"
	"router/internal/tokens"
	"router/internal/upstream"
)

// maxUpstreamBodyBytes caps how much of a non-streaming upstream response
// we'll buffer. At the spec's ~42k token average request (~170KB) this is
// generous headroom for the large tail without being unbounded.
const maxUpstreamBodyBytes = 32 << 20

// stepResult is the outcome of one dispatch attempt against one upstream.
type stepResult int

const (
	stepSuccess stepResult = iota
	stepRetry
	stepClientGone
)

// serveNonStream sends one attempt and, on success, writes the full
// sanitized response to the client. It returns stepRetry for any failure
// that hasn't written anything to the client yet (safe to fail over) and
// stepClientGone when the failure is actually the caller's context having
// been cancelled (client disconnect or budget exhausted). The second return
// value is the number of tokens actually delivered to the client on
// stepSuccess (0 otherwise) — computed locally from this response's own
// usage object, never by diffing the upstream's shared TokensSent window,
// which would race against other concurrent requests to the same upstream.
func serveNonStream(ctx context.Context, w http.ResponseWriter, up *upstream.Upstream, clientReq *jsonutil.OrderedMap, publicModel, reqID string, rec *stats.Recorder, logger *slog.Logger) (stepResult, int64) {
	start := time.Now()
	resp, err := dispatch(ctx, up, clientReq)
	if err != nil {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		if ctx.Err() != nil {
			return stepClientGone, 0
		}
		logger.Warn("upstream dispatch failed", "upstream", up.Cfg.ID, "error", err)
		return stepRetry, 0
	}
	defer resp.Body.Close()
	rec.RecordTTFT(up.Cfg.ID, time.Since(start))

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBodyBytes))
	if err != nil {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		if ctx.Err() != nil {
			return stepClientGone, 0
		}
		logger.Warn("upstream response read failed", "upstream", up.Cfg.ID, "error", err)
		return stepRetry, 0
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		logger.Warn("upstream returned error status", "upstream", up.Cfg.ID, "status", resp.StatusCode)
		return stepRetry, 0
	}

	parsed, err := jsonutil.ParseObject(bodyBytes)
	if err != nil {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		logger.Warn("upstream returned malformed JSON", "upstream", up.Cfg.ID, "error", err)
		return stepRetry, 0
	}

	var delivered int64
	if usageRaw, ok := parsed.Get("usage"); ok {
		if u, ok := tokens.ParseUsage(usageRaw); ok {
			delivered = int64(u.TotalTokens)
			up.RecordDelivered(delivered)
		}
	}

	sanitize.Rewrite(parsed, publicModel, reqID)
	sanitized, err := parsed.MarshalJSON()
	if err != nil {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		return stepRetry, 0
	}

	up.Breaker.RecordResult(true, func() { up.Debt.Store(0) })
	rec.RecordOutcome(up.Cfg.ID, true)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sanitized)
	return stepSuccess, delivered
}
