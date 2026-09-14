// Package config loads, validates, and hot-reloads the router's YAML configuration.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so it can be parsed from YAML strings like "90s".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalYAML() (interface{}, error) {
	return d.Duration.String(), nil
}

type ServerConfig struct {
	Listen         string   `yaml:"listen"`
	MaxAttempts    int      `yaml:"max_attempts"`
	RequestBudget  Duration `yaml:"request_budget"`
	StatsAuthToken string   `yaml:"stats_auth_token"`
}

type PublicModelUpstream struct {
	ID       string  `yaml:"id"`
	Weight   float64 `yaml:"weight"`
	Fallback bool    `yaml:"fallback"`
}

type PublicModel struct {
	Name      string                `yaml:"name"`
	Upstreams []PublicModelUpstream `yaml:"upstreams"`
}

type Quirks struct {
	Strip               []string `yaml:"strip"`
	NoTemperature       bool     `yaml:"no_temperature"`
	NoTopP              bool     `yaml:"no_top_p"`
	MaxCompletionTokens bool     `yaml:"max_completion_tokens"`
	StopAsString        bool     `yaml:"stop_as_string"`
}

type Upstream struct {
	ID                 string   `yaml:"id"`
	BaseURL            string   `yaml:"base_url"`
	APIKeyEnv          string   `yaml:"api_key_env"`
	Model              string   `yaml:"model"`
	ContextWindow      int      `yaml:"context_window"`
	MaxOutput          int      `yaml:"max_output"`
	Modalities         []string `yaml:"modalities"`
	SupportsTools      bool     `yaml:"supports_tools"`
	SupportsJSONSchema bool     `yaml:"supports_json_schema"`
	TPMLimit           int64    `yaml:"tpm_limit"`
	Quirks             Quirks   `yaml:"quirks"`
}

type RedisConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`
}

type Config struct {
	Server       ServerConfig  `yaml:"server"`
	PublicModels []PublicModel `yaml:"public_models"`
	Upstreams    []Upstream    `yaml:"upstreams"`
	Redis        RedisConfig   `yaml:"redis"`
}

// UpstreamByID returns the upstream definition for id, if present.
func (c *Config) UpstreamByID(id string) (*Upstream, bool) {
	for i := range c.Upstreams {
		if c.Upstreams[i].ID == id {
			return &c.Upstreams[i], true
		}
	}
	return nil, false
}

// PublicModelByName returns the public model definition for name, if present.
func (c *Config) PublicModelByName(name string) (*PublicModel, bool) {
	for i := range c.PublicModels {
		if c.PublicModels[i].Name == name {
			return &c.PublicModels[i], true
		}
	}
	return nil, false
}

// APIKey resolves the upstream's API key from its configured environment variable.
func (u *Upstream) APIKey() string {
	if u.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(u.APIKeyEnv)
}

// Load reads and validates a config file from disk.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return Parse(data)
}

// Parse validates and returns a Config from raw YAML bytes.
func Parse(data []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	applyDefaults(&c)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func applyDefaults(c *Config) {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.MaxAttempts == 0 {
		c.Server.MaxAttempts = 3
	}
	if c.Server.RequestBudget.Duration == 0 {
		c.Server.RequestBudget.Duration = 90 * time.Second
	}
}

// Validate checks referential integrity and sane values. It never mutates c.
func (c *Config) Validate() error {
	if c.Server.Listen == "" {
		return fmt.Errorf("server.listen is required")
	}
	if c.Server.MaxAttempts < 1 {
		return fmt.Errorf("server.max_attempts must be >= 1")
	}
	if c.Server.RequestBudget.Duration <= 0 {
		return fmt.Errorf("server.request_budget must be > 0")
	}

	if len(c.Upstreams) == 0 {
		return fmt.Errorf("at least one upstream must be configured")
	}
	seenUpstream := map[string]bool{}
	for i, u := range c.Upstreams {
		if u.ID == "" {
			return fmt.Errorf("upstreams[%d]: id is required", i)
		}
		if seenUpstream[u.ID] {
			return fmt.Errorf("upstreams[%d]: duplicate id %q", i, u.ID)
		}
		seenUpstream[u.ID] = true
		if u.BaseURL == "" {
			return fmt.Errorf("upstream %q: base_url is required", u.ID)
		}
		if u.Model == "" {
			return fmt.Errorf("upstream %q: model is required", u.ID)
		}
		if u.ContextWindow <= 0 {
			return fmt.Errorf("upstream %q: context_window must be > 0", u.ID)
		}
		if u.MaxOutput <= 0 {
			return fmt.Errorf("upstream %q: max_output must be > 0", u.ID)
		}
		if len(u.Modalities) == 0 {
			return fmt.Errorf("upstream %q: modalities must not be empty", u.ID)
		}
		if u.TPMLimit <= 0 {
			return fmt.Errorf("upstream %q: tpm_limit must be > 0", u.ID)
		}
	}

	if len(c.PublicModels) == 0 {
		return fmt.Errorf("at least one public model must be configured")
	}
	seenModel := map[string]bool{}
	for i, m := range c.PublicModels {
		if m.Name == "" {
			return fmt.Errorf("public_models[%d]: name is required", i)
		}
		if seenModel[m.Name] {
			return fmt.Errorf("public_models[%d]: duplicate name %q", i, m.Name)
		}
		seenModel[m.Name] = true
		if len(m.Upstreams) == 0 {
			return fmt.Errorf("public model %q: at least one upstream is required", m.Name)
		}
		hasFallback := false
		var weightSum float64
		seenRef := map[string]bool{}
		for j, ref := range m.Upstreams {
			if ref.ID == "" {
				return fmt.Errorf("public model %q upstreams[%d]: id is required", m.Name, j)
			}
			if seenRef[ref.ID] {
				return fmt.Errorf("public model %q: duplicate upstream ref %q", m.Name, ref.ID)
			}
			seenRef[ref.ID] = true
			if _, ok := seenUpstream[ref.ID]; !ok {
				return fmt.Errorf("public model %q: references unknown upstream %q", m.Name, ref.ID)
			}
			if ref.Weight < 0 {
				return fmt.Errorf("public model %q upstream %q: weight must be >= 0", m.Name, ref.ID)
			}
			weightSum += ref.Weight
			if ref.Fallback {
				hasFallback = true
			}
		}
		if !hasFallback {
			return fmt.Errorf("public model %q: must designate at least one fallback upstream", m.Name)
		}
		if weightSum <= 0 {
			return fmt.Errorf("public model %q: sum of weights must be > 0", m.Name)
		}
	}

	if c.Redis.Enabled && c.Redis.URL == "" {
		return fmt.Errorf("redis.url is required when redis.enabled is true")
	}

	return nil
}
