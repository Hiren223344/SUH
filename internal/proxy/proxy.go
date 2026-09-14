// Package proxy implements the HTTP-facing request handling: parsing and
// validating client requests, running the gate/select/dispatch/retry loop,
// and writing sanitized responses back to the client.
package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"router/internal/jsonutil"
	"router/internal/openai"
	"router/internal/router"
	"router/internal/stats"
	"router/internal/tokens"
)

const maxClientBodyBytes = 32 << 20

// deliverySyncer is implemented by redisync.Syncer. Kept as a narrow local
// interface so this package doesn't need to import redisync (and so the
// field can simply be left nil when Redis is disabled — the default,
// fully-correct single-instance path).
type deliverySyncer interface {
	RecordLocalDelivery(model, upstreamID string, tokens float64)
}

// Handlers bundles the dependencies every HTTP handler in this package needs.
type Handlers struct {
	Registry *router.Registry
	Stats    *stats.Recorder
	Logger   *slog.Logger
	Syncer   deliverySyncer // optional; nil when redis.enabled is false
}

// ChatCompletions serves POST /v1/chat/completions.
func (h *Handlers) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	reqID := newRequestID()
	w.Header().Set("X-Request-Id", reqID)

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxClientBodyBytes))
	if err != nil {
		writeFatal(w, http.StatusBadRequest, "failed to read request body", openai.ErrTypeInvalidRequest)
		return
	}
	clientReq, err := jsonutil.ParseObject(bodyBytes)
	if err != nil {
		writeFatal(w, http.StatusBadRequest, "invalid JSON request body", openai.ErrTypeInvalidRequest)
		return
	}

	cfg := h.Registry.Config()
	publicModel, _ := clientReq.GetString("model")
	if publicModel == "" {
		writeFatal(w, http.StatusBadRequest, "\"model\" is required", openai.ErrTypeInvalidRequest)
		return
	}
	pm, ok := cfg.PublicModelByName(publicModel)
	if !ok {
		writeFatal(w, http.StatusNotFound, fmt.Sprintf("The model '%s' does not exist", publicModel), openai.ErrTypeNotFound)
		return
	}

	signals, err := openai.ExtractSignals(clientReq)
	if err != nil {
		writeFatal(w, http.StatusBadRequest, "invalid request: "+err.Error(), openai.ErrTypeInvalidRequest)
		return
	}

	_, _, totalEst := tokens.EstimateTotal(signals)

	budget := cfg.Server.RequestBudget.Duration
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()

	maxAttempts := cfg.Server.MaxAttempts
	tried := map[string]bool{}
	attempted := 0

	for attempt := 0; attempt < maxAttempts; attempt++ {
		gin := router.GateInput{Signals: signals, EstimatedTotalTokens: int64(totalEst)}
		candidates := router.Candidates(pm, h.Registry, gin, tried)
		if len(candidates) == 0 {
			if attempted == 0 {
				writeFatal(w, http.StatusBadRequest, "no configured upstream can serve this request (modality, tool, structured-output, or context-window constraints)", openai.ErrTypeInvalidRequest)
				return
			}
			break
		}

		selected := router.Select(candidates)
		allowed, _ := selected.Breaker.Allow()
		if !allowed {
			tried[selected.Cfg.ID] = true
			continue
		}

		tried[selected.Cfg.ID] = true
		attempted++
		selected.ReserveTPM(int64(totalEst))
		selected.InFlight.Add(1)

		if signals.Stream {
			before := selected.TokensSent.Sum()
			result, committed := serveStream(ctx, w, selected, clientReq, publicModel, reqID, h.Stats, h.Logger)
			selected.InFlight.Add(-1)
			if committed {
				delivered := selected.TokensSent.Sum() - before
				router.ApplyDelivery(pm, candidates, selected, delivered)
				if h.Syncer != nil {
					h.Syncer.RecordLocalDelivery(publicModel, selected.Cfg.ID, float64(delivered))
				}
				return
			}
			if result == stepClientGone {
				return
			}
			continue
		}

		before := selected.TokensSent.Sum()
		result := serveNonStream(ctx, w, selected, clientReq, publicModel, reqID, h.Stats, h.Logger)
		selected.InFlight.Add(-1)
		switch result {
		case stepSuccess:
			delivered := selected.TokensSent.Sum() - before
			router.ApplyDelivery(pm, candidates, selected, delivered)
			if h.Syncer != nil {
				h.Syncer.RecordLocalDelivery(publicModel, selected.Cfg.ID, float64(delivered))
			}
			return
		case stepClientGone:
			return
		default:
			continue
		}
	}

	if r.Context().Err() != nil {
		return // client disconnected; nothing to write
	}
	if ctx.Err() != nil {
		writeFatal(w, http.StatusGatewayTimeout, "request exceeded time budget", openai.ErrTypeTimeout)
		return
	}
	writeFatal(w, http.StatusBadGateway, "all upstream attempts failed", openai.ErrTypeAPIError)
}
