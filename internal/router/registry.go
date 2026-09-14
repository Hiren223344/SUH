// Package router implements the two-stage routing engine: Stage 1 hard
// capability/health gating, and Stage 2 token-debt-weighted selection.
package router

import (
	"sync"

	"router/internal/config"
	"router/internal/upstream"
)

// Registry holds live runtime state (circuit breaker, sliding windows,
// in-flight counters) for every configured upstream, keyed by id. Runtime
// state survives config hot-reloads for upstreams that remain present —
// only the config pointer is swapped — so a weight change never resets an
// upstream's breaker or drift history.
type Registry struct {
	mgr  *config.Manager
	bcfg upstream.BreakerConfig

	mu        sync.RWMutex
	upstreams map[string]*upstream.Upstream
	cfg       *config.Config
}

func NewRegistry(mgr *config.Manager, bcfg upstream.BreakerConfig) *Registry {
	r := &Registry{mgr: mgr, bcfg: bcfg, upstreams: map[string]*upstream.Upstream{}}
	r.Sync(mgr.Get())
	mgr.Subscribe(r.Sync)
	return r
}

func (r *Registry) Get(id string) *upstream.Upstream {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.upstreams[id]
}

// Sync reconciles runtime upstream state against cfg: new upstream ids get
// fresh runtime state, upstreams still present keep their breaker/window/
// in-flight state but pick up the latest config fields, and removed
// upstreams are dropped.
func (r *Registry) Sync(cfg *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
	seen := make(map[string]bool, len(cfg.Upstreams))
	for i := range cfg.Upstreams {
		uc := &cfg.Upstreams[i]
		seen[uc.ID] = true
		if existing, ok := r.upstreams[uc.ID]; ok {
			existing.Cfg = uc
			continue
		}
		r.upstreams[uc.ID] = upstream.New(uc, r.bcfg)
	}
	for id := range r.upstreams {
		if !seen[id] {
			delete(r.upstreams, id)
		}
	}
}

// Config returns the config snapshot this registry last synced against.
func (r *Registry) Config() *config.Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

// All returns every known upstream's runtime state, for /internal/stats.
func (r *Registry) All() []*upstream.Upstream {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*upstream.Upstream, 0, len(r.upstreams))
	for _, u := range r.upstreams {
		out = append(out, u)
	}
	return out
}
