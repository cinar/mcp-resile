// Package config loads and validates mcp-resile.yaml, per spec.md §7.
//
// V1 only parses the server: section and the backends: list; auth,
// policies, and telemetry are wired in by later features. Missing or
// malformed required fields are rejected here, at load time, rather than
// surfacing as a confusing failure on the first request. Field validation
// is delegated to github.com/cinar/checker via struct tags; the one rule
// checker can't express — that prefixes must be distinct across backends —
// is checked separately, since checker only validates within one struct.
package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cinar/checker"
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

// errUnsupportedServerTransport and errUnsupportedBackendTransport back the
// checker.Register'd checkers below.
var (
	errUnsupportedServerTransport  = fmt.Errorf("unsupported transport, only %q is supported in v1", transportStreamableHTTP)
	errUnsupportedBackendTransport = fmt.Errorf("unsupported transport, only %q is supported in v1", backendTransportHTTP)
)

func init() {
	checker.Register("server-transport", checker.MakeRegexpMaker("^"+transportStreamableHTTP+"$", errUnsupportedServerTransport))
	checker.Register("backend-transport", checker.MakeRegexpMaker("^"+backendTransportHTTP+"$", errUnsupportedBackendTransport))
}

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
	Listen           string   `yaml:"listen" checkers:"required"`
	Transport        string   `yaml:"transport" checkers:"server-transport"`
	ReadTimeout      Duration `yaml:"read_timeout" checkers:"min:1"`
	WriteTimeout     Duration `yaml:"write_timeout" checkers:"min:1"`
	MaxRequestBytes  int64    `yaml:"max_request_bytes" checkers:"min:1"`
	MaxResponseBytes int64    `yaml:"max_response_bytes" checkers:"min:1"`
}

// Backend is one entry of the backends: list.
type Backend struct {
	ID        string `yaml:"id" checkers:"required"`
	Transport string `yaml:"transport" checkers:"backend-transport"`
	Prefix    string `yaml:"prefix"`
	URL       string `yaml:"url" checkers:"required url"`
}

// Config is the top-level mcp-resile.yaml document.
type Config struct {
	Version  string    `yaml:"version"`
	Server   Server    `yaml:"server"`
	Backends []Backend `yaml:"backends" checkers:"required"`
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

	cfg.applyDefaults()

	// checker.Check only recurses into struct-kind fields, so the Backends
	// slice itself is checked here (for the required, non-empty rule), and
	// each of its elements is checked separately in the loop below.
	if errs, ok := checker.Check(&cfg); !ok {
		return nil, checkerError("", errs)
	}
	for i := range cfg.Backends {
		if errs, ok := checker.Check(&cfg.Backends[i]); !ok {
			return nil, checkerError(fmt.Sprintf("Backends[%d].", i), errs)
		}
	}

	if err := validatePrefixes(cfg.Backends); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validatePrefixes enforces spec.md §5.1's namespace conflict resolution:
// once more than one backend is configured, each must carry a distinct,
// non-empty prefix so gateway-side tool names can never collide. A lone
// backend may leave prefix empty and pass its tools through unprefixed.
// This compares across list elements, so it can't be a checker struct tag
// (checker only validates fields within one struct at a time).
func validatePrefixes(backends []Backend) error {
	if len(backends) < 2 {
		return nil
	}

	seenBy := make(map[string]string, len(backends))
	for _, b := range backends {
		if b.Prefix == "" {
			return fmt.Errorf("backend %q: prefix is required when more than one backend is configured", b.ID)
		}
		if owner, ok := seenBy[b.Prefix]; ok {
			return fmt.Errorf("backends %q and %q: duplicate prefix %q", owner, b.ID, b.Prefix)
		}
		seenBy[b.Prefix] = b.ID
	}

	return nil
}

// applyDefaults fills in optional fields left unset in the YAML, before
// validation runs, so an omitted transport or timeout is never mistaken for
// an invalid one. It also means the min:1 checkers on the timeout/byte-size
// fields only ever see the zero value as "unset, now defaulted", never as
// "explicitly zero" — an explicit negative value is what they catch.
func (c *Config) applyDefaults() {
	if c.Server.Transport == "" {
		c.Server.Transport = transportStreamableHTTP
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = Duration(defaultReadTimeout)
	}
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = Duration(defaultWriteTimeout)
	}
	if c.Server.MaxRequestBytes == 0 {
		c.Server.MaxRequestBytes = defaultMaxRequestBytes
	}
	if c.Server.MaxResponseBytes == 0 {
		c.Server.MaxResponseBytes = defaultMaxResponseBytes
	}

	for i := range c.Backends {
		if c.Backends[i].Transport == "" {
			c.Backends[i].Transport = backendTransportHTTP
		}
	}
}

// checkerError flattens a checker.Errors map into a single, deterministic
// error message, with each field name prefixed for context.
func checkerError(prefix string, errs checker.Errors) error {
	names := make([]string, 0, len(errs))
	for name := range errs {
		names = append(names, name)
	}
	sort.Strings(names)

	msgs := make([]string, len(names))
	for i, name := range names {
		msgs[i] = fmt.Sprintf("%s%s: %v", prefix, name, errs[name])
	}
	return errors.New(strings.Join(msgs, "; "))
}
