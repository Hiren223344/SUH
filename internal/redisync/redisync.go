// Package redisync provides optional cross-instance convergence for the
// router's debt-based selection. It is entirely additive: with Redis
// absent (or redis.enabled: false), every other package works exactly as
// documented for a single instance. This package only nudges local debt
// accumulators toward the cluster-wide delivery history.
//
// Design: each instance applies its own deliveries to its local debt
// accumulators immediately and synchronously (via router.ApplyDelivery, in
// the request path — see the proxy package). Separately, on an interval,
// each instance (a) publishes its own delivered-token contribution since
// the last sync as a delta into a shared cumulative counter per
// (public_model, upstream), and (b) reads back how much OTHER instances
// have contributed since the last sync, applying only that remote delta
// locally — so no instance double-counts its own deliveries.
//
// Cross-instance deltas are credited against the full configured upstream
// pool for the public model (not a per-request gated candidate set, which
// isn't available outside the request that produced it) — a documented
// simplification: remote nudges use full-pool target weights rather than
// per-request renormalized weights.
package redisync

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"router/internal/router"
	"router/internal/upstream"
)

// counterStore is the minimal cumulative-counter interface this package
// needs from Redis, kept narrow so the sync math can be unit tested
// without a live Redis server.
type counterStore interface {
	IncrByFloat(ctx context.Context, key string, delta float64) (float64, error)
	Get(ctx context.Context, key string) (float64, error)
}

// Syncer periodically reconciles local debt accumulators against a shared
// cumulative counter in Redis.
type Syncer struct {
	store    counterStore
	interval time.Duration
	logger   *slog.Logger

	mu              sync.Mutex
	localSinceSync  map[string]float64 // key -> tokens delivered locally since last sync, not yet published
	lastKnownGlobal map[string]float64 // key -> global counter value as of last successful sync
}

func NewSyncer(store counterStore, interval time.Duration, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{
		store:           store,
		interval:        interval,
		logger:          logger,
		localSinceSync:  map[string]float64{},
		lastKnownGlobal: map[string]float64{},
	}
}

func key(model, upstreamID string) string {
	return fmt.Sprintf("router:tokens:%s:%s", model, upstreamID)
}

// RecordLocalDelivery must be called (in addition to, not instead of, the
// local router.ApplyDelivery already applied synchronously in the request
// path) every time this instance delivers tokens, so the next sync tick
// knows how much of the global counter is this instance's own
// contribution and must not be re-applied to itself as if it were remote.
func (s *Syncer) RecordLocalDelivery(model, upstreamID string, tokens float64) {
	if tokens <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.localSinceSync[key(model, upstreamID)] += tokens
}

// SyncOnce publishes this instance's pending local contributions and
// applies any newly-observed remote contributions to local debt, for every
// (public_model, upstream) pair in the current config.
func (s *Syncer) SyncOnce(ctx context.Context, reg *router.Registry) {
	cfg := reg.Config()
	for i := range cfg.PublicModels {
		pm := &cfg.PublicModels[i]
		var candidates []*upstream.Upstream
		for _, ref := range pm.Upstreams {
			if up := reg.Get(ref.ID); up != nil {
				candidates = append(candidates, up)
			}
		}
		for _, ref := range pm.Upstreams {
			s.syncOne(ctx, pm.Name, ref.ID, candidates, reg)
		}
	}
}

func (s *Syncer) syncOne(ctx context.Context, model, upstreamID string, candidates []*upstream.Upstream, reg *router.Registry) {
	k := key(model, upstreamID)

	s.mu.Lock()
	pending := s.localSinceSync[k]
	s.mu.Unlock()

	var newGlobal float64
	var err error
	if pending > 0 {
		newGlobal, err = s.store.IncrByFloat(ctx, k, pending)
	} else {
		newGlobal, err = s.store.Get(ctx, k)
	}
	if err != nil {
		s.logger.Warn("redisync: sync failed", "key", k, "error", err)
		return
	}

	s.mu.Lock()
	prevGlobal := s.lastKnownGlobal[k]
	s.localSinceSync[k] -= pending
	s.lastKnownGlobal[k] = newGlobal
	s.mu.Unlock()

	// Of the increase in the global counter since last sync, `pending` is
	// this instance's own contribution (already reflected locally at
	// delivery time). Only the remainder is genuinely remote.
	remoteDelta := (newGlobal - prevGlobal) - pending
	if remoteDelta <= 0 {
		return
	}

	winner := reg.Get(upstreamID)
	if winner == nil || len(candidates) == 0 {
		return
	}

	cfg := reg.Config()
	pmPtr, ok := cfg.PublicModelByName(model)
	if !ok {
		return
	}
	router.ApplyDelivery(pmPtr, candidates, winner, int64(remoteDelta))
}

// Run blocks, syncing on the configured interval until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context, reg *router.Registry) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SyncOnce(ctx, reg)
		}
	}
}
