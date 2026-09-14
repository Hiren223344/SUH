package redisync

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"router/internal/config"
	"router/internal/router"
	"router/internal/upstream"
)

// fakeStore is an in-memory counterStore, standing in for Redis so the
// sync/delta arithmetic can be tested deterministically without a live
// server — this project's stack constraints forbid any DB/broker
// dependency beyond Redis itself, and pulling in a Redis-in-process
// library just for tests isn't justified for one small interface.
type fakeStore struct {
	mu     sync.Mutex
	values map[string]float64
}

func newFakeStore() *fakeStore { return &fakeStore{values: map[string]float64{}} }

func (f *fakeStore) IncrByFloat(ctx context.Context, key string, delta float64) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] += delta
	return f.values[key], nil
}

func (f *fakeStore) Get(ctx context.Context, key string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.values[key], nil
}

const twoUpstreamYAML = `
server:
  listen: ":0"
  max_attempts: 3
  request_budget: 5s
public_models:
  - name: "M"
    upstreams:
      - id: up1
        weight: 0.5
      - id: up2
        weight: 0.5
        fallback: true
upstreams:
  - id: up1
    base_url: "http://localhost"
    model: "m1"
    context_window: 1000
    max_output: 100
    modalities: [text]
    tpm_limit: 1000000
  - id: up2
    base_url: "http://localhost"
    model: "m2"
    context_window: 1000
    max_output: 100
    modalities: [text]
    tpm_limit: 1000000
`

func newTestRegistry(t *testing.T) *router.Registry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(twoUpstreamYAML), 0644))
	mgr, err := config.NewManager(path, nil)
	require.NoError(t, err)
	return router.NewRegistry(mgr, upstream.DefaultBreakerConfig())
}

// TestSyncer_RemoteDeliveryCreditsWithoutDoubleCounting is the core
// correctness property: instance A's own delivery, once published, must
// not be re-applied to instance A's local debt as if it were a remote
// event — only genuinely-other instances' contributions should nudge it.
func TestSyncer_RemoteDeliveryCreditsWithoutDoubleCounting(t *testing.T) {
	store := newFakeStore()
	regA := newTestRegistry(t)
	regB := newTestRegistry(t)

	syncerA := NewSyncer(store, 0, nil)
	syncerB := NewSyncer(store, 0, nil)

	ctx := context.Background()

	// Instance A delivers 1000 tokens on up1, applied locally already
	// (simulated by directly nudging its own debt the way proxy.go would).
	up1A := regA.Get("up1")
	up1A.RecordDelivered(1000)
	syncerA.RecordLocalDelivery("M", "up1", 1000)

	debtBeforeSync := up1A.Debt.Load()

	// A syncs first: publishes its 1000, sees no remote delta yet (global
	// == its own pending contribution).
	syncerA.SyncOnce(ctx, regA)
	require.Equal(t, debtBeforeSync, up1A.Debt.Load(), "an instance must not re-apply its own just-published delivery as a remote delta")

	// Instance B delivers 500 tokens on up2, publishes it.
	up2B := regB.Get("up2")
	up2B.RecordDelivered(500)
	syncerB.RecordLocalDelivery("M", "up2", 500)
	syncerB.SyncOnce(ctx, regB)

	// Now A syncs again: should observe B's 500 on up2 as a genuine remote
	// delivery and apply it to A's local view.
	up1AbeforeRemote := up1A.Debt.Load()
	syncerA.SyncOnce(ctx, regA)
	require.NotEqual(t, up1AbeforeRemote, up1A.Debt.Load(), "instance A should have absorbed instance B's remote delivery into its local debt")

	// A syncing again with nothing new published should be a no-op.
	stable := up1A.Debt.Load()
	syncerA.SyncOnce(ctx, regA)
	require.Equal(t, stable, up1A.Debt.Load())
}

func TestSyncer_GetOnEmptyKeyIsZeroNotError(t *testing.T) {
	store := newFakeStore()
	v, err := store.Get(context.Background(), "router:tokens:M:nonexistent")
	require.NoError(t, err)
	require.Equal(t, float64(0), v)
}
