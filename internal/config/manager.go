package config

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// Manager holds a live, hot-reloadable Config. Readers call Get(); it is
// always safe to call concurrently with Reload/watch.
//
// Hot reload is implemented by polling the file's mtime rather than fsnotify:
// fsnotify is not in the project's allowed dependency list, and a 10s poll
// is explicitly sanctioned by the spec as an alternative.
type Manager struct {
	path     string
	interval time.Duration
	cur      atomic.Pointer[Config]
	lastMod  time.Time
	logger   *slog.Logger
}

// NewManager loads the config at path once (returning any load error) and
// prepares a Manager that can poll for subsequent changes.
func NewManager(path string, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	m := &Manager{path: path, interval: 10 * time.Second, logger: logger}
	m.cur.Store(cfg)
	if fi, err := os.Stat(path); err == nil {
		m.lastMod = fi.ModTime()
	}
	return m, nil
}

// Get returns the currently active config. The returned pointer is
// immutable; callers must not mutate it.
func (m *Manager) Get() *Config {
	return m.cur.Load()
}

// SetPollInterval overrides the default 10s poll interval. Must be called
// before Watch.
func (m *Manager) SetPollInterval(d time.Duration) {
	m.interval = d
}

// Reload re-reads and validates the config file, atomically swapping it in
// on success. A broken config is logged and the previous config remains
// active — reload never crashes the process and never drops in-flight
// requests, since Get() callers only ever see a fully-formed Config.
func (m *Manager) Reload() error {
	cfg, err := Load(m.path)
	if err != nil {
		m.logger.Error("config reload failed, keeping previous config", "path", m.path, "error", err)
		return err
	}
	m.cur.Store(cfg)
	m.logger.Info("config reloaded", "path", m.path)
	return nil
}

// Watch polls the config file for changes until ctx is cancelled.
func (m *Manager) Watch(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fi, err := os.Stat(m.path)
			if err != nil {
				m.logger.Warn("config stat failed", "path", m.path, "error", err)
				continue
			}
			if fi.ModTime().After(m.lastMod) {
				m.lastMod = fi.ModTime()
				_ = m.Reload()
			}
		}
	}
}
