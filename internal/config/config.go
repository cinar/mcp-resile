// Package config loads and validates mcp-resile.yaml, per spec.md §7.
//
// V1 parses the server: section, the backends: list, and the policies:
// list's tool_pattern field (the rest of each policy's validation/resilience
// fields are added by the features that consume them — FEATURE-011 onward);
// auth and telemetry are wired in by later features. Missing or malformed
// required fields are rejected here, at load time, rather than surfacing as
// a confusing failure on the first request. Field validation is delegated
// to github.com/cinar/checker via struct tags; rules checker can't express
// — that prefixes must be distinct across backends, and that tool_pattern
// must be a well-formed glob — are checked separately, since checker only
// validates within one struct and has no glob-syntax checker.
package config

import (
	"errors"
	"fmt"
	"os"
	"path"
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

// CircuitBreaker is the resilience.circuit_breaker: sub-block of a policy
// (spec.md §7), mapping directly onto github.com/cinar/resile/circuit.Config
// (FEATURE-011). A zero value for any field leaves resile's own circuit.New
// defaults in effect (50% failure rate, 60s window, 60s reset) — checker
// can't validate these (it only auto-recurses into non-pointer struct
// fields, and this one is a pointer so config.Policy can tell "circuit
// breaker omitted" apart from "circuit breaker present with defaults"), so
// validateCircuitBreaker checks them by hand.
type CircuitBreaker struct {
	FailureRate    float64  `yaml:"failure_rate"`
	WindowDuration Duration `yaml:"window_duration"`
	ResetTimeout   Duration `yaml:"reset_timeout"`
}

// Resilience is the resilience: sub-block of a policy (spec.md §7). Later
// features (FEATURE-012 onward) add retries/rate_limit/timeout as they
// start consuming them.
type Resilience struct {
	CircuitBreaker *CircuitBreaker `yaml:"circuit_breaker"`
}

// Policy is one entry of the policies: list (spec.md §7). A tool name is
// matched against ToolPattern (a path.Match glob) to resolve the policy
// that governs it, per FEATURE-010. Later features add the remaining
// validation/resilience fields as they start consuming them.
type Policy struct {
	ToolPattern string     `yaml:"tool_pattern" checkers:"required"`
	Resilience  Resilience `yaml:"resilience"`
}

// Config is the top-level mcp-resile.yaml document.
type Config struct {
	Version  string    `yaml:"version"`
	Server   Server    `yaml:"server"`
	Backends []Backend `yaml:"backends" checkers:"required"`
	Policies []Policy  `yaml:"policies"`
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
	for i := range cfg.Policies {
		if errs, ok := checker.Check(&cfg.Policies[i]); !ok {
			return nil, checkerError(fmt.Sprintf("Policies[%d].", i), errs)
		}
	}

	if err := validatePrefixes(cfg.Backends); err != nil {
		return nil, err
	}
	if err := validatePolicies(cfg.Policies); err != nil {
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

// validatePolicies checks that every policy's tool_pattern is a well-formed
// glob per path.Match's syntax, so a malformed pattern is rejected here, at
// load time, rather than silently never matching (or erroring) on the first
// request that reaches policy.Resolver.Resolve. path.Match's error depends
// only on the pattern, not the name being matched against, so matching
// against "" is enough to surface a syntax error.
func validatePolicies(policies []Policy) error {
	for _, p := range policies {
		if _, err := path.Match(p.ToolPattern, ""); err != nil {
			return fmt.Errorf("policy %q: invalid tool_pattern: %w", p.ToolPattern, err)
		}
		if err := validateCircuitBreaker(p.Resilience.CircuitBreaker); err != nil {
			return fmt.Errorf("policy %q: %w", p.ToolPattern, err)
		}
	}
	return nil
}

// validateCircuitBreaker rejects an explicit, out-of-range value instead of
// letting it silently fall back to resile's own circuit.New default (e.g. a
// negative or >100 failure_rate quietly becomes 50.0) — a config mistake
// should fail loudly at startup, not produce an unexplained default in
// production. A zero value is left alone: it means "unset, use resile's
// default", the same convention Server's timeout/byte-size fields use.
func validateCircuitBreaker(cb *CircuitBreaker) error {
	if cb == nil {
		return nil
	}
	if cb.FailureRate < 0 || cb.FailureRate > 100 {
		return fmt.Errorf("circuit_breaker.failure_rate: must be between 0 and 100, got %v", cb.FailureRate)
	}
	if cb.WindowDuration < 0 {
		return fmt.Errorf("circuit_breaker.window_duration: must not be negative")
	}
	if cb.ResetTimeout < 0 {
		return fmt.Errorf("circuit_breaker.reset_timeout: must not be negative")
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
