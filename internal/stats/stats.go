// Package stats tracks per-upstream outcome/latency metrics beyond what
// upstream.Upstream already exposes (breaker state, sliding-window token
// sum, in-flight count), and serves the auth-gated /internal/stats endpoint.
package stats

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"router/internal/config"
	"router/internal/router"
	"router/internal/upstream"
)

const ttftSampleCap = 512

type upstreamMetrics struct {
	success atomic.Int64
	failure atomic.Int64

	mu          sync.Mutex
	ttftSamples []float64 // milliseconds, capped ring buffer
	ttftNext    int
}

func (m *upstreamMetrics) recordTTFT(d time.Duration) {
	ms := float64(d.Microseconds()) / 1000
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.ttftSamples) < ttftSampleCap {
		m.ttftSamples = append(m.ttftSamples, ms)
		return
	}
	m.ttftSamples[m.ttftNext] = ms
	m.ttftNext = (m.ttftNext + 1) % ttftSampleCap
}

func (m *upstreamMetrics) percentiles() (p50, p95 float64) {
	m.mu.Lock()
	samples := append([]float64(nil), m.ttftSamples...)
	m.mu.Unlock()
	if len(samples) == 0 {
		return 0, 0
	}
	sort.Float64s(samples)
	return percentile(samples, 0.50), percentile(samples, 0.95)
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// Recorder accumulates outcome/latency metrics per upstream id.
type Recorder struct {
	mu      sync.RWMutex
	metrics map[string]*upstreamMetrics
}

func NewRecorder() *Recorder {
	return &Recorder{metrics: map[string]*upstreamMetrics{}}
}

func (r *Recorder) get(id string) *upstreamMetrics {
	r.mu.RLock()
	m, ok := r.metrics[id]
	r.mu.RUnlock()
	if ok {
		return m
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.metrics[id]; ok {
		return m
	}
	m = &upstreamMetrics{}
	r.metrics[id] = m
	return m
}

// RecordOutcome records one dispatched request's success/failure.
func (r *Recorder) RecordOutcome(upstreamID string, success bool) {
	m := r.get(upstreamID)
	if success {
		m.success.Add(1)
	} else {
		m.failure.Add(1)
	}
}

// RecordTTFT records a time-to-first-content-chunk sample.
func (r *Recorder) RecordTTFT(upstreamID string, d time.Duration) {
	r.get(upstreamID).recordTTFT(d)
}

// UpstreamStat is one row of the /internal/stats response.
type UpstreamStat struct {
	PublicModel     string  `json:"public_model"`
	UpstreamID      string  `json:"upstream_id"`
	TargetShare     float64 `json:"target_share"`
	RealizedShare1h float64 `json:"realized_share_1h"`
	TokensSent1h    int64   `json:"tokens_sent_1h"`
	DriftDelta      float64 `json:"drift_delta"`
	SuccessCount    int64   `json:"success_count"`
	FailureCount    int64   `json:"failure_count"`
	SuccessRate     float64 `json:"success_rate"`
	TTFTP50Ms       float64 `json:"ttft_p50_ms"`
	TTFTP95Ms       float64 `json:"ttft_p95_ms"`
	CircuitState    string  `json:"circuit_state"`
	InFlight        int64   `json:"in_flight"`
}

type StatsResponse struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Upstreams   []UpstreamStat `json:"upstreams"`
}

// Snapshot builds the full stats response by joining each public model's
// configured weights against live registry/recorder state.
func (r *Recorder) Snapshot(reg *router.Registry) StatsResponse {
	cfg := reg.Config()
	resp := StatsResponse{GeneratedAt: time.Now()}

	for _, pm := range cfg.PublicModels {
		var totalWeight float64
		var totalSent int64
		type row struct {
			up     *upstream.Upstream
			weight float64
			sent   int64
		}
		var rows []row
		for _, ref := range pm.Upstreams {
			up := reg.Get(ref.ID)
			if up == nil {
				continue
			}
			sent := up.TokensSent.Sum()
			totalWeight += ref.Weight
			totalSent += sent
			rows = append(rows, row{up: up, weight: ref.Weight, sent: sent})
		}
		for _, rw := range rows {
			var target, realized float64
			if totalWeight > 0 {
				target = rw.weight / totalWeight
			}
			if totalSent > 0 {
				realized = float64(rw.sent) / float64(totalSent)
			}
			m := r.get(rw.up.Cfg.ID)
			success := m.success.Load()
			failure := m.failure.Load()
			var successRate float64
			if success+failure > 0 {
				successRate = float64(success) / float64(success+failure)
			}
			p50, p95 := m.percentiles()
			resp.Upstreams = append(resp.Upstreams, UpstreamStat{
				PublicModel:     pm.Name,
				UpstreamID:      rw.up.Cfg.ID,
				TargetShare:     target,
				RealizedShare1h: realized,
				TokensSent1h:    rw.sent,
				DriftDelta:      target - realized,
				SuccessCount:    success,
				FailureCount:    failure,
				SuccessRate:     successRate,
				TTFTP50Ms:       p50,
				TTFTP95Ms:       p95,
				CircuitState:    rw.up.Breaker.State().String(),
				InFlight:        rw.up.InFlight.Load(),
			})
		}
	}
	return resp
}

// Handler serves GET /internal/stats, gated by a bearer token from
// server.stats_auth_token. If no token is configured, the endpoint refuses
// all requests rather than defaulting open.
func Handler(reg *router.Registry, rec *Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		cfg := reg.Config()
		if !authorized(cfg, req) {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rec.Snapshot(reg))
	}
}

func authorized(cfg *config.Config, req *http.Request) bool {
	token := cfg.Server.StatsAuthToken
	if token == "" {
		return false
	}
	got := req.Header.Get("Authorization")
	return got == "Bearer "+token
}
