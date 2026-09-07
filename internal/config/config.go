// Package config loads and validates mcp-resile.yaml, per spec.md §7.
//
// V1 parses the server: section, the backends: list, and the policies:
// list's tool_pattern field (the rest of each policy's validation/resilience
// fields are added by the features that consume them — FEATURE-011 onward);
// auth and telemetry are wired in by later features. Missing or malformed
// required fields are rejected here, at load time, rather than surfacing as
// a confusing failure on the first request. Field validation is delegated
// to github.com/cinar/checker via struct tags; rules checker can't express
// — that prefixes/ids must be distinct across backends, that tool_pattern
// must be a well-formed glob, and that an unrecognized YAML key anywhere in
// the document is rejected rather than silently ignored (FEATURE-022) —
// are checked separately, since checker only validates within one struct
// and has no glob-syntax or unknown-field checker of its own.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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

// Retries is the resilience.retries: sub-block of a policy (spec.md §7),
// mapping onto resile.WithMaxAttempts/WithBackoff (FEATURE-012). Like
// CircuitBreaker, it's a pointer so "retries omitted" and "retries present
// with defaults" stay distinguishable, and so validateRetries/applyDefaults
// handle it by hand rather than via checker's struct-field auto-recursion.
type Retries struct {
	MaxAttempts uint     `yaml:"max_attempts"`
	BaseDelay   Duration `yaml:"base_delay"`
	MaxDelay    Duration `yaml:"max_delay"`
	Backoff     string   `yaml:"backoff"`
}

// RateLimit is the resilience.rate_limit: sub-block of a policy (spec.md
// §7), mapping onto resile.NewRateLimiter (FEATURE-013). Unlike
// CircuitBreaker/Retries, there's no sensible zero-value default: resile's
// token bucket rejects every request at Rate 0, and divides by zero
// internally at Interval 0 — so validateRateLimit requires both whenever
// rate_limit: is present, rather than silently defaulting either.
type RateLimit struct {
	Rate     float64  `yaml:"rate"`
	Interval Duration `yaml:"interval"`
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
	MinDeadlineThreshold Duration        `yaml:"min_deadline_threshold"`
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
// default, like Auth.
type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Port    int    `yaml:"port"`
	Path    string `yaml:"path"`
}

// Logging is the telemetry.logging: sub-block of the config (spec.md §7),
// controlling the gateway's structured slog output (FEATURE-021). Level is
// one of "debug"/"info"/"warn"/"error"; Format is "json" or "text".
type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Telemetry is the telemetry: section of the config (spec.md §7).
type Telemetry struct {
	Metrics Metrics `yaml:"metrics"`
	Logging Logging `yaml:"logging"`
}

// Config is the top-level mcp-resile.yaml document.
type Config struct {
	Version   string    `yaml:"version"`
	Server    Server    `yaml:"server"`
	Auth      Auth      `yaml:"auth"`
	Telemetry Telemetry `yaml:"telemetry"`
	Backends  []Backend `yaml:"backends" checkers:"required"`
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

	if err := validateAuth(cfg.Auth); err != nil {
		return nil, err
	}
	if err := validateTelemetry(cfg.Telemetry); err != nil {
		return nil, err
	}
	if err := validateLogging(cfg.Telemetry.Logging); err != nil {
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

// validateTelemetry rejects an explicitly out-of-range port instead of
// letting it silently reach net.Listen and fail there with a less useful
// error; an unset port is left alone (applyDefaults fills it in whenever
// metrics are enabled).
func validateTelemetry(t Telemetry) error {
	if !t.Metrics.Enabled {
		return nil
	}
	if t.Metrics.Port < 0 || t.Metrics.Port > 65535 {
		return fmt.Errorf("telemetry.metrics.port: must be between 0 and 65535, got %d", t.Metrics.Port)
	}
	if t.Metrics.Path != "" && !strings.HasPrefix(t.Metrics.Path, "/") {
		return fmt.Errorf("telemetry.metrics.path: must start with %q, got %q", "/", t.Metrics.Path)
	}
	return nil
}

// validLogLevels/Formats are the only values telemetry.logging.level/format
// accept, per spec.md §7's comment on the field ("debug, info, warn, error"
// / "json, text").
var (
	validLogLevels  = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	validLogFormats = map[string]bool{"json": true, "text": true}
)

// validateLogging rejects a level/format outside spec.md §7's documented
// set, so a typo (e.g. "warning" instead of "warn") fails loudly at load
// time instead of silently falling back to logging/New's own default.
func validateLogging(l Logging) error {
	if !validLogLevels[l.Level] {
		return fmt.Errorf("telemetry.logging.level: unsupported level %q, must be one of debug/info/warn/error", l.Level)
	}
	if !validLogFormats[l.Format] {
		return fmt.Errorf("telemetry.logging.format: unsupported format %q, must be one of json/text", l.Format)
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
		if err := validateRetries(p.Resilience.Retries); err != nil {
			return fmt.Errorf("policy %q: %w", p.ToolPattern, err)
		}
		if err := validateRateLimit(p.Resilience.RateLimit); err != nil {
			return fmt.Errorf("policy %q: %w", p.ToolPattern, err)
		}
		if p.Resilience.MinDeadlineThreshold < 0 {
			return fmt.Errorf("policy %q: resilience.min_deadline_threshold: must not be negative", p.ToolPattern)
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

// validateRetries checks fields left after applyDefaults has already filled
// in the zero ones, so what's left to reject is only an explicit mistake: a
// negative delay, or a backoff other than the one resile currently supports.
func validateRetries(r *Retries) error {
	if r == nil {
		return nil
	}
	if r.BaseDelay < 0 {
		return fmt.Errorf("retries.base_delay: must not be negative")
	}
	if r.MaxDelay < 0 {
		return fmt.Errorf("retries.max_delay: must not be negative")
	}
	if r.Backoff != backoffFullJitter {
		return fmt.Errorf("retries.backoff: unsupported backoff %q, only %q is supported in v1", r.Backoff, backoffFullJitter)
	}
	return nil
}

// validateRateLimit requires both fields whenever rate_limit: is present:
// unlike the other resilience blocks, there's no zero value that means
// "unset, use a sensible default" here — resile.NewRateLimiter(0, x) simply
// rejects every request, and interval 0 divides by zero internally.
func validateRateLimit(rl *RateLimit) error {
	if rl == nil {
		return nil
	}
	if rl.Rate <= 0 {
		return fmt.Errorf("rate_limit.rate: must be greater than 0, got %v", rl.Rate)
	}
	if rl.Interval <= 0 {
		return fmt.Errorf("rate_limit.interval: must be greater than 0")
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
