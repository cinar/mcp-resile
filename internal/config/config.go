// Package config loads and validates mcp-resile.yaml, per spec.md §7.
//
// V1 only parses the server: section and a single-entry backends: list;
// auth, policies, and telemetry are wired in by later features. Missing or
// malformed required fields are rejected here, at load time, rather than
// surfacing as a confusing failure on the first request.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// V1 only supports one ingress transport and one egress transport.
const (
	transportStreamableHTTP = "streamable-http"
	backendTransportHTTP    = "http"

	defaultReadTimeout      = 30 * time.Second
	defaultWriteTimeout     = 30 * time.Second
	defaultMaxRequestBytes  = 1 << 20   // 1 MiB
	defaultMaxResponseBytes = 512 << 10 // 512 KiB
)

// Duration wraps time.Duration to unmarshal from YAML duration strings
// (e.g. "30s"); time.Duration has no native YAML text encoding.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Server is the server: section of the config.
type Server struct {
	Listen           string   `yaml:"listen"`
	Transport        string   `yaml:"transport"`
	ReadTimeout      Duration `yaml:"read_timeout"`
	WriteTimeout     Duration `yaml:"write_timeout"`
	MaxRequestBytes  int64    `yaml:"max_request_bytes"`
	MaxResponseBytes int64    `yaml:"max_response_bytes"`
}

// Backend is one entry of the backends: list.
type Backend struct {
	ID        string `yaml:"id"`
	Transport string `yaml:"transport"`
	Prefix    string `yaml:"prefix"`
	URL       string `yaml:"url"`
}

// Config is the top-level mcp-resile.yaml document.
type Config struct {
	Version  string    `yaml:"version"`
	Server   Server    `yaml:"server"`
	Backends []Backend `yaml:"backends"`
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse validates and returns the config encoded in data.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := cfg.Server.validate(); err != nil {
		return nil, err
	}
	if err := cfg.validateBackends(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (s *Server) validate() error {
	if s.Listen == "" {
		return fmt.Errorf("server.listen is required")
	}

	if s.Transport == "" {
		s.Transport = transportStreamableHTTP
	} else if s.Transport != transportStreamableHTTP {
		return fmt.Errorf("server.transport: unsupported transport %q, only %q is supported in v1", s.Transport, transportStreamableHTTP)
	}

	if s.ReadTimeout == 0 {
		s.ReadTimeout = Duration(defaultReadTimeout)
	}
	if s.WriteTimeout == 0 {
		s.WriteTimeout = Duration(defaultWriteTimeout)
	}
	if s.MaxRequestBytes == 0 {
		s.MaxRequestBytes = defaultMaxRequestBytes
	}
	if s.MaxResponseBytes == 0 {
		s.MaxResponseBytes = defaultMaxResponseBytes
	}
	return nil
}

// validateBackends enforces the v1 restriction of exactly one backend.
// Multi-backend routing arrives with FEATURE-007.
func (c *Config) validateBackends() error {
	if len(c.Backends) != 1 {
		return fmt.Errorf("backends: v1 supports exactly one backend, got %d", len(c.Backends))
	}

	b := &c.Backends[0]
	if b.ID == "" {
		return fmt.Errorf("backends[0].id is required")
	}
	if b.URL == "" {
		return fmt.Errorf("backends[0].url is required")
	}
	if b.Transport == "" {
		b.Transport = backendTransportHTTP
	} else if b.Transport != backendTransportHTTP {
		return fmt.Errorf("backends[0].transport: unsupported transport %q, only %q is supported in v1", b.Transport, backendTransportHTTP)
	}
	return nil
}
