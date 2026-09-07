// Package config loads and validates mcp-resile.yaml, per spec.md §7.
//
// V1 parses the server: section, the backends: list, and the policies:
// list's tool_pattern field (the rest of each policy's validation/resilience
// fields are added by the features that consume them — FEATURE-011 onward);
// auth and telemetry are wired in by later features. Missing or malformed
// required fields are rejected here, at load time, rather than surfacing as
// a confusing failure on the first request. Field validation is delegated
// to github.com/cinar/checker/v2 via struct tags — including into every
// nested resilience.* pointer sub-block (circuit_breaker/retries/
// rate_limit), which checker v2's CheckStruct can recurse into automatically
// (v1 could not: it only auto-recursed into non-pointer struct fields). What
// checker still can't express — that prefixes/ids must be distinct across
// backends, that tool_pattern must be a well-formed glob, and that an
// unrecognized YAML key anywhere in the document is rejected rather than
// silently ignored (FEATURE-022) — is checked separately, since those are
// either cross-element comparisons or outside checker's domain entirely.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	checker "github.com/cinar/checker/v2"
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

	// defaultAuthHeader matches spec.md §7's own example; auth.header only
	// gets defaulted when auth.enabled: is true, since it's meaningless
	// otherwise.
	defaultAuthHeader = "Authorization"

	// defaultMetricsPort/Path match spec.md §7's own example; like
	// auth.header, they're only defaulted when telemetry.metrics.enabled:
	// is true.
	defaultMetricsPort = 9090
	defaultMetricsPath = "/metrics"

	// defaultLogLevel/Format match spec.md §7's own example. Unlike
	// metrics/auth, telemetry.logging: has no enabled: switch — the gateway
	// always logs somewhere, so these are defaulted unconditionally.
	defaultLogLevel  = "info"
	defaultLogFormat = "json"

	// backoffFullJitter is the only backoff resile currently provides
	// (resile.NewFullJitter); it's still a named config value, not
	// hardcoded, since it's what spec.md §7's backoff: field documents.
	backoffFullJitter = "full_jitter"

	// defaultRetryMaxAttempts/BaseDelay/MaxDelay mirror resile.DefaultConfig's
	// own defaults, reused here rather than inventing separate numbers.
	defaultRetryMaxAttempts = 5
	defaultRetryBaseDelay   = 100 * time.Millisecond
	defaultRetryMaxDelay    = 30 * time.Second
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
	Listen           string   `yaml:"listen" checkers:"required"`
	Transport        string   `yaml:"transport" checkers:"oneof:streamable-http"`
	ReadTimeout      Duration `yaml:"read_timeout" checkers:"gte:1"`
	WriteTimeout     Duration `yaml:"write_timeout" checkers:"gte:1"`
	MaxRequestBytes  int64    `yaml:"max_request_bytes" checkers:"gte:1"`
	MaxResponseBytes int64    `yaml:"max_response_bytes" checkers:"gte:1"`
}

// Backend is one entry of the backends: list.
type Backend struct {
	ID        string `yaml:"id" checkers:"required"`
	Transport string `yaml:"transport" checkers:"oneof:http"`
	Prefix    string `yaml:"prefix"`
	URL       string `yaml:"url" checkers:"required url"`
}

// CircuitBreaker is the resilience.circuit_breaker: sub-block of a policy
// (spec.md §7), mapping directly onto github.com/cinar/resile/circuit.Config
// (FEATURE-011). A zero value for any field leaves resile's own circuit.New
// defaults in effect (50% failure rate, 60s window, 60s reset). It's a
// pointer field on Resilience so config.Policy can tell "circuit breaker
// omitted" apart from "circuit breaker present with defaults" — but unlike
// v1's checker, v2's CheckStruct dereferences a non-nil pointer field and
// recurses into it just like any other struct (a nil one is left alone), so
// FailureRate/WindowDuration/ResetTimeout's own checkers tags are enough;
// there's no more a validateCircuitBreaker hand-check to keep in sync with
// them.
type CircuitBreaker struct {
	FailureRate    float64  `yaml:"failure_rate" checkers:"gte:0 lte:100"`
	WindowDuration Duration `yaml:"window_duration" checkers:"gte:0"`
	ResetTimeout   Duration `yaml:"reset_timeout" checkers:"gte:0"`
}

// Retries is the resilience.retries: sub-block of a policy (spec.md §7),
// mapping onto resile.WithMaxAttempts/WithBackoff (FEATURE-012). Like
// CircuitBreaker, it's a pointer so "retries omitted" and "retries present
// with defaults" stay distinguishable; applyDefaults still fills in its
// zero fields by hand (including Backoff, before oneof below ever sees it —
// an empty Backoff would otherwise fail oneof rather than be defaulted).
// MaxAttempts carries no checkers tag: it's a uint (never negative to begin
// with), and applyDefaults already replaces an explicit 0 with
// defaultRetryMaxAttempts, so no value ever reaches validation for it to
// reject.
type Retries struct {
	MaxAttempts uint     `yaml:"max_attempts"`
	BaseDelay   Duration `yaml:"base_delay" checkers:"gte:0"`
	MaxDelay    Duration `yaml:"max_delay" checkers:"gte:0"`
	Backoff     string   `yaml:"backoff" checkers:"oneof:full_jitter"`
}

// RateLimit is the resilience.rate_limit: sub-block of a policy (spec.md
// §7), mapping onto resile.NewRateLimiter (FEATURE-013). Unlike
// CircuitBreaker/Retries, there's no sensible zero-value default: resile's
// token bucket rejects every request at Rate 0, and divides by zero
// internally at Interval 0 — so both fields require a strictly positive
// value whenever rate_limit: is present at all (a nil RateLimit is skipped
// entirely, same as CircuitBreaker/Retries).
type RateLimit struct {
	Rate     float64  `yaml:"rate" checkers:"gt:0"`
	Interval Duration `yaml:"interval" checkers:"gt:0"`
}

// Resilience is the resilience: sub-block of a policy (spec.md §7).
// MinDeadlineThreshold maps onto resile.WithMinDeadlineThreshold
// (FEATURE-015): a scalar duration, not a pointer sub-block like the others,
// so its zero value means "unconfigured" directly — proxy.resilienceOptions
// passes it to resile as-is either way (see its doc comment for why).
//
// Timeout maps onto resile.WithTimeout, per spec.md §7's own example — but
// no shipped FEATURE ever adopted it as an acceptance criterion (only
// min_deadline_threshold, FEATURE-015, did), so it isn't wired into
// proxy.resilienceOptions. It's still parsed rather than silently dropped:
// validatePolicies rejects a non-zero value with a clear "not supported in
// v1" error (FEATURE-022) instead of accepting a config field that would
// silently do nothing.
type Resilience struct {
	CircuitBreaker       *CircuitBreaker `yaml:"circuit_breaker"`
	Retries              *Retries        `yaml:"retries"`
	RateLimit            *RateLimit      `yaml:"rate_limit"`
	MinDeadlineThreshold Duration        `yaml:"min_deadline_threshold" checkers:"gte:0"`
	Timeout              Duration        `yaml:"timeout"`
}

// Validation is the validation: sub-block of a policy (spec.md §7, §5.2).
// Like Resilience.Timeout, it's parsed (so it's recognized rather than
// rejected as an unknown field) but not enforced: FEATURE-017 shipped only
// inputSchema validation, not reject_unknown_fields/max_argument_bytes (see
// its own commit message) — validatePolicies rejects a config that actually
// sets these with a clear "not supported in v1" error.
type Validation struct {
	RejectUnknownFields bool  `yaml:"reject_unknown_fields"`
	MaxArgumentBytes    int64 `yaml:"max_argument_bytes"`
}

// Policy is one entry of the policies: list (spec.md §7). A tool name is
// matched against ToolPattern (a path.Match glob) to resolve the policy
// that governs it, per FEATURE-010. Later features add the remaining
// validation/resilience fields as they start consuming them.
type Policy struct {
	ToolPattern string     `yaml:"tool_pattern" checkers:"required"`
	Validation  Validation `yaml:"validation"`
	Resilience  Resilience `yaml:"resilience"`
}

// Auth is the auth: section of the config (spec.md §5.4, §7): a bearer
// token or API key allow-list checked against one HTTP header on every
// ingress request, rejecting anything not on the list before it reaches
// any backend (FEATURE-019). It answers "is this caller allowed to talk to
// the gateway at all" — not per-tool masking, which is Post-V1 (spec.md
// §11).
type Auth struct {
	Enabled bool     `yaml:"enabled"`
	Header  string   `yaml:"header"`
	Tokens  []string `yaml:"tokens"`
}

// Metrics is the telemetry.metrics: sub-block of the config (spec.md §7),
// controlling the Prometheus /metrics endpoint (FEATURE-020). It's served on
// its own listener (Port), separate from server.listen, so a scraper never
// competes with MCP traffic for the same port or routes. Disabled by
// default, like Auth. Port/Path's checkers tags apply unconditionally
// (there's no "only when Enabled" gate), which is fine even when Metrics
// isn't enabled: Port's zero value (0) and Path's default ("/metrics",
// filled in by applyDefaults regardless of Enabled) both already satisfy
// their own range/prefix checks.
type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Port    int    `yaml:"port" checkers:"gte:0 lte:65535"`
	Path    string `yaml:"path" checkers:"omitempty starts-with:/"`
}

// Logging is the telemetry.logging: sub-block of the config (spec.md §7),
// controlling the gateway's structured slog output (FEATURE-021). Level is
// one of "debug"/"info"/"warn"/"error"; Format is "json" or "text". Both are
// always defaulted to a valid value by applyDefaults before checking runs,
// so oneof always has something to check against.
type Logging struct {
	Level  string `yaml:"level" checkers:"oneof:debug,info,warn,error"`
	Format string `yaml:"format" checkers:"oneof:json,text"`
}

// Telemetry is the telemetry: section of the config (spec.md §7).
type Telemetry struct {
	Metrics Metrics `yaml:"metrics"`
	Logging Logging `yaml:"logging"`
}

// Config is the top-level mcp-resile.yaml document. Backends' "@" prefix
// applies min-len to the slice itself (at least one backend), not to each
// item — checker v2's own convention for telling a container-level check
// apart from an item-level one (see the README's "Slice and Item Level
// Checkers" section). min-len, not required, because Required's zero-value
// check (IsZero) only catches a nil slice, not an explicitly empty
// "backends: []" — min-len counts actual elements either way.
type Config struct {
	Version   string    `yaml:"version"`
	Server    Server    `yaml:"server"`
	Auth      Auth      `yaml:"auth"`
	Telemetry Telemetry `yaml:"telemetry"`
	Backends  []Backend `yaml:"backends" checkers:"@min-len:1"`
	Policies  []Policy  `yaml:"policies"`
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

	// KnownFields rejects any YAML key that doesn't map to a field on its
	// corresponding struct (e.g. a typo'd "polcies:", or "resilience.timout")
	// as a decode error naming the field and line number, rather than
	// yaml.Unmarshal's default of silently ignoring it — a misspelled
	// section would otherwise load successfully with that whole section
	// simply missing, which is a worse failure mode than a loud one
	// (FEATURE-022).
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		// io.EOF means data was empty (or all comments/whitespace): leave cfg
		// at its zero value and let the required-field checks below produce
		// a clear error, rather than surfacing the low-level EOF here.
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.applyDefaults()

	// CheckStruct walks the whole config tree in one pass — every struct
	// field, including down into each Backends/Policies element and every
	// non-nil resilience.* pointer sub-block (FEATURE-011/012/013) — so,
	// unlike v1's checker, there's no need to loop over cfg.Backends/
	// cfg.Policies by hand just to check each element separately. Its
	// CheckErrors return value already implements error (formatting as
	// sorted "field.path: message" pairs), so it's returned directly.
	if errs, ok := checker.CheckStruct(&cfg); !ok {
		return nil, errs
	}

	if err := validateAuth(cfg.Auth); err != nil {
		return nil, err
	}
	if err := validateBackendIDs(cfg.Backends); err != nil {
		return nil, err
	}
	if err := validatePrefixes(cfg.Backends); err != nil {
		return nil, err
	}
	if err := validatePolicies(cfg.Policies); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validateAuth requires at least one token whenever auth.enabled: is true —
// an empty allow-list would reject every single request, which is never
// what an operator setting enabled: true actually wants, so it fails loudly
// at load time instead of locking everyone out at runtime.
func validateAuth(auth Auth) error {
	if !auth.Enabled {
		return nil
	}
	if len(auth.Tokens) == 0 {
		return errors.New("auth.tokens: at least one token is required when auth.enabled is true")
	}
	return nil
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

// validateBackendIDs requires every backend's id to be unique, regardless
// of backend count (unlike prefix, which is only required/checked once
// there's more than one backend). A duplicate id is always a config
// mistake — most likely a copy-pasted backend entry with the prefix/url
// edited but the id left behind — and it would otherwise surface only as
// confusing duplicate "backend" labels in logs/metrics (FEATURE-020,
// FEATURE-021), never as a load-time error (FEATURE-022).
func validateBackendIDs(backends []Backend) error {
	seenAt := make(map[string]int, len(backends))
	for i, b := range backends {
		if first, ok := seenAt[b.ID]; ok {
			return fmt.Errorf("backends[%d] and backends[%d]: duplicate id %q", first, i, b.ID)
		}
		seenAt[b.ID] = i
	}
	return nil
}

// validatePolicies checks what checker's struct-tag-driven CheckStruct pass
// can't: that every policy's tool_pattern is a well-formed glob per
// path.Match's syntax (path.Match's error depends only on the pattern, not
// the name being matched against, so matching against "" is enough to
// surface a syntax error), and that the two config fields no shipped
// FEATURE ever wired up — resilience.timeout and validation: (see their own
// doc comments) — are left unset, rather than accepted and silently
// ignored. Neither rejection fits a checker tag: eq's string-only
// comparison would panic on Timeout's Duration/int64 kind, and there's no
// "must be the zero value" checker for a whole struct like Validation.
func validatePolicies(policies []Policy) error {
	for _, p := range policies {
		if _, err := path.Match(p.ToolPattern, ""); err != nil {
			return fmt.Errorf("policy %q: invalid tool_pattern: %w", p.ToolPattern, err)
		}
		if p.Resilience.Timeout != 0 {
			return fmt.Errorf("policy %q: resilience.timeout: not supported in v1 (use min_deadline_threshold)", p.ToolPattern)
		}
		if p.Validation != (Validation{}) {
			return fmt.Errorf("policy %q: validation: not supported in v1 (only the tool's own inputSchema is validated)", p.ToolPattern)
		}
	}
	return nil
}

// applyDefaults fills in optional fields left unset in the YAML, before
// validation runs, so an omitted transport or timeout is never mistaken for
// an invalid one. It also means the gte:1 checkers on the timeout/byte-size
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

	if c.Auth.Enabled && c.Auth.Header == "" {
		c.Auth.Header = defaultAuthHeader
	}

	if c.Telemetry.Metrics.Enabled {
		if c.Telemetry.Metrics.Port == 0 {
			c.Telemetry.Metrics.Port = defaultMetricsPort
		}
		if c.Telemetry.Metrics.Path == "" {
			c.Telemetry.Metrics.Path = defaultMetricsPath
		}
	}

	if c.Telemetry.Logging.Level == "" {
		c.Telemetry.Logging.Level = defaultLogLevel
	}
	if c.Telemetry.Logging.Format == "" {
		c.Telemetry.Logging.Format = defaultLogFormat
	}
	if c.Server.MaxResponseBytes == 0 {
		c.Server.MaxResponseBytes = defaultMaxResponseBytes
	}

	for i := range c.Backends {
		if c.Backends[i].Transport == "" {
			c.Backends[i].Transport = backendTransportHTTP
		}
	}

	for i := range c.Policies {
		r := c.Policies[i].Resilience.Retries
		if r == nil {
			continue
		}
		if r.MaxAttempts == 0 {
			r.MaxAttempts = defaultRetryMaxAttempts
		}
		if r.BaseDelay == 0 {
			r.BaseDelay = Duration(defaultRetryBaseDelay)
		}
		if r.MaxDelay == 0 {
			r.MaxDelay = Duration(defaultRetryMaxDelay)
		}
		if r.Backoff == "" {
			r.Backoff = backoffFullJitter
		}
	}
}
