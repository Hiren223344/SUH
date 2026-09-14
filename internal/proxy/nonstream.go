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
// been cancelled (client disconnect or budget exhausted).
func serveNonStream(ctx context.Context, w http.ResponseWriter, up *upstream.Upstream, clientReq *jsonutil.OrderedMap, publicModel, reqID string, rec *stats.Recorder, logger *slog.Logger) stepResult {
	start := time.Now()
	resp, err := dispatch(ctx, up, clientReq)
	if err != nil {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		if ctx.Err() != nil {
			return stepClientGone
		}
		logger.Warn("upstream dispatch failed", "upstream", up.Cfg.ID, "error", err)
		return stepRetry
	}
	defer resp.Body.Close()
	rec.RecordTTFT(up.Cfg.ID, time.Since(start))

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBodyBytes))
	if err != nil {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		if ctx.Err() != nil {
			return stepClientGone
		}
		logger.Warn("upstream response read failed", "upstream", up.Cfg.ID, "error", err)
		return stepRetry
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		logger.Warn("upstream returned error status", "upstream", up.Cfg.ID, "status", resp.StatusCode)
		return stepRetry
	}

	parsed, err := jsonutil.ParseObject(bodyBytes)
	if err != nil {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		logger.Warn("upstream returned malformed JSON", "upstream", up.Cfg.ID, "error", err)
		return stepRetry
	}

	if usageRaw, ok := parsed.Get("usage"); ok {
		if u, ok := tokens.ParseUsage(usageRaw); ok {
			up.RecordDelivered(int64(u.TotalTokens))
		}
	}

	sanitize.Rewrite(parsed, publicModel, reqID)
	sanitized, err := parsed.MarshalJSON()
	if err != nil {
		up.Breaker.RecordResult(false, nil)
		rec.RecordOutcome(up.Cfg.ID, false)
		return stepRetry
	}

	up.Breaker.RecordResult(true, func() { up.Debt.Store(0) })
	rec.RecordOutcome(up.Cfg.ID, true)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sanitized)
	return stepSuccess
}
