package stats

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"router/internal/config"
	"router/internal/router"
	"router/internal/upstream"
)

const testConfigYAML = `
server:
  listen: ":0"
  max_attempts: 3
  request_budget: 5s
  stats_auth_token: "secret-token"
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

func testRegistry(t *testing.T) *router.Registry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(testConfigYAML), 0644))
	mgr, err := config.NewManager(path, nil)
	require.NoError(t, err)
	return router.NewRegistry(mgr, upstream.DefaultBreakerConfig())
}

func TestHandler_RefusesWithoutToken(t *testing.T) {
	reg := testRegistry(t)
	rec := NewRecorder()
	h := Handler(reg, rec)

	req := httptest.NewRequest("GET", "/internal/stats", nil)
	rw := httptest.NewRecorder()
	h(rw, req)
	require.Equal(t, 401, rw.Code)
}

func TestHandler_RefusesWrongToken(t *testing.T) {
	reg := testRegistry(t)
	rec := NewRecorder()
	h := Handler(reg, rec)

	req := httptest.NewRequest("GET", "/internal/stats", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rw := httptest.NewRecorder()
	h(rw, req)
	require.Equal(t, 401, rw.Code)
}

func TestHandler_AllowsCorrectToken(t *testing.T) {
	reg := testRegistry(t)
	rec := NewRecorder()
	h := Handler(reg, rec)

	req := httptest.NewRequest("GET", "/internal/stats", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rw := httptest.NewRecorder()
	h(rw, req)
	require.Equal(t, 200, rw.Code)
}

func TestSnapshot_ReportsTargetAndRealizedShare(t *testing.T) {
	reg := testRegistry(t)
	rec := NewRecorder()

	up1 := reg.Get("up1")
	up1.RecordDelivered(300)
	up2 := reg.Get("up2")
	up2.RecordDelivered(700)
	rec.RecordOutcome("up1", true)
	rec.RecordOutcome("up2", true)
	rec.RecordOutcome("up2", false)

	snap := rec.Snapshot(reg)
	require.Len(t, snap.Upstreams, 2)

	byID := map[string]UpstreamStat{}
	for _, s := range snap.Upstreams {
		byID[s.UpstreamID] = s
	}
	require.InDelta(t, 0.5, byID["up1"].TargetShare, 0.001)
	require.InDelta(t, 0.3, byID["up1"].RealizedShare1h, 0.001)
	require.Equal(t, int64(1), byID["up2"].SuccessCount)
	require.Equal(t, int64(1), byID["up2"].FailureCount)
	require.InDelta(t, 0.5, byID["up2"].SuccessRate, 0.001)
	require.Equal(t, "closed", byID["up1"].CircuitState)
}
