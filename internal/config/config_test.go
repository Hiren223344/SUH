package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const validYAML = `
server:
  listen: ":8080"
  max_attempts: 3
  request_budget: 90s

public_models:
  - name: "M"
    upstreams:
      - id: up1
        weight: 0.6
      - id: up2
        weight: 0.4
        fallback: true

upstreams:
  - id: up1
    base_url: "https://example.com/v1"
    model: "upstream-model-1"
    context_window: 128000
    max_output: 4096
    modalities: [text]
    tpm_limit: 1000000
  - id: up2
    base_url: "https://example2.com/v1"
    model: "upstream-model-2"
    context_window: 64000
    max_output: 4096
    modalities: [text]
    tpm_limit: 1000000
`

func TestParse_ValidConfig(t *testing.T) {
	cfg, err := Parse([]byte(validYAML))
	require.NoError(t, err)
	require.Equal(t, ":8080", cfg.Server.Listen)
	require.Equal(t, 90*time.Second, cfg.Server.RequestBudget.Duration)
	require.Len(t, cfg.PublicModels, 1)
}

func TestParse_RejectsUnknownUpstreamReference(t *testing.T) {
	bad := validYAML + "\n" // baseline valid; now corrupt a reference
	broken := `
public_models:
  - name: "M"
    upstreams:
      - id: does-not-exist
        weight: 1
        fallback: true
upstreams:
  - id: up1
    base_url: "https://example.com/v1"
    model: "m"
    context_window: 1000
    max_output: 100
    modalities: [text]
    tpm_limit: 1000
`
	_, err := Parse([]byte(broken))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown upstream")
	_ = bad
}

func TestParse_RequiresFallback(t *testing.T) {
	broken := `
public_models:
  - name: "M"
    upstreams:
      - id: up1
        weight: 1
upstreams:
  - id: up1
    base_url: "https://example.com/v1"
    model: "m"
    context_window: 1000
    max_output: 100
    modalities: [text]
    tpm_limit: 1000
`
	_, err := Parse([]byte(broken))
	require.Error(t, err)
	require.Contains(t, err.Error(), "fallback")
}

func TestParse_RejectsZeroWeightSum(t *testing.T) {
	broken := `
public_models:
  - name: "M"
    upstreams:
      - id: up1
        weight: 0
        fallback: true
upstreams:
  - id: up1
    base_url: "https://example.com/v1"
    model: "m"
    context_window: 1000
    max_output: 100
    modalities: [text]
    tpm_limit: 1000
`
	_, err := Parse([]byte(broken))
	require.Error(t, err)
}

func TestManager_ReloadPicksUpWeightChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0644))

	mgr, err := NewManager(path, nil)
	require.NoError(t, err)
	require.InDelta(t, 0.6, mustWeight(mgr.Get(), "up1"), 0.0001)

	updated := `
server:
  listen: ":8080"
  max_attempts: 3
  request_budget: 90s
public_models:
  - name: "M"
    upstreams:
      - id: up1
        weight: 0.1
      - id: up2
        weight: 0.9
        fallback: true
upstreams:
  - id: up1
    base_url: "https://example.com/v1"
    model: "upstream-model-1"
    context_window: 128000
    max_output: 4096
    modalities: [text]
    tpm_limit: 1000000
  - id: up2
    base_url: "https://example2.com/v1"
    model: "upstream-model-2"
    context_window: 64000
    max_output: 4096
    modalities: [text]
    tpm_limit: 1000000
`
	require.NoError(t, os.WriteFile(path, []byte(updated), 0644))
	require.NoError(t, mgr.Reload())
	require.InDelta(t, 0.1, mustWeight(mgr.Get(), "up1"), 0.0001)
}

func TestManager_ReloadKeepsPreviousConfigOnBrokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0644))

	mgr, err := NewManager(path, nil)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(path, []byte("not: valid: yaml: at: all: ["), 0644))
	require.Error(t, mgr.Reload())

	// The previous, valid config must still be live — a broken config file
	// must never crash the process or drop the active config.
	require.InDelta(t, 0.6, mustWeight(mgr.Get(), "up1"), 0.0001)
}

func TestManager_SubscribersNotifiedOnReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0644))

	mgr, err := NewManager(path, nil)
	require.NoError(t, err)

	notified := false
	mgr.Subscribe(func(c *Config) { notified = true })

	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0644))
	require.NoError(t, mgr.Reload())
	require.True(t, notified)
}

func mustWeight(c *Config, upstreamID string) float64 {
	for _, pm := range c.PublicModels {
		for _, u := range pm.Upstreams {
			if u.ID == upstreamID {
				return u.Weight
			}
		}
	}
	return -1
}
