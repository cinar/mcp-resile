package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cinar/mcp-resile/internal/config"
)

const validYAML = `
version: "v1"
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "database-service"
    prefix: "db_"
    url: "http://db-primary.internal:9000/mcp"
`

func TestParseValidAppliesDefaults(t *testing.T) {
	cfg, err := config.Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if cfg.Server.Listen != "0.0.0.0:8080" {
		t.Errorf("Server.Listen = %q, want %q", cfg.Server.Listen, "0.0.0.0:8080")
	}
	if cfg.Server.Transport != "streamable-http" {
		t.Errorf("Server.Transport = %q, want default %q", cfg.Server.Transport, "streamable-http")
	}
	if time.Duration(cfg.Server.ReadTimeout) != 30*time.Second {
		t.Errorf("Server.ReadTimeout = %v, want default 30s", time.Duration(cfg.Server.ReadTimeout))
	}
	if cfg.Server.MaxRequestBytes != 1<<20 {
		t.Errorf("Server.MaxRequestBytes = %d, want default %d", cfg.Server.MaxRequestBytes, 1<<20)
	}

	if len(cfg.Backends) != 1 {
		t.Fatalf("len(Backends) = %d, want 1", len(cfg.Backends))
	}
	b := cfg.Backends[0]
	if b.ID != "database-service" || b.URL != "http://db-primary.internal:9000/mcp" {
		t.Errorf("Backends[0] = %+v, want id/url from YAML", b)
	}
	if b.Transport != "http" {
		t.Errorf("Backends[0].Transport = %q, want default %q", b.Transport, "http")
	}
}

func TestParseExplicitTimeoutsAndSizes(t *testing.T) {
	yaml := `
server:
  listen: "0.0.0.0:8080"
  read_timeout: "5s"
  write_timeout: "10s"
  max_request_bytes: 2048
  max_response_bytes: 4096
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
`
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if time.Duration(cfg.Server.ReadTimeout) != 5*time.Second {
		t.Errorf("ReadTimeout = %v, want 5s", time.Duration(cfg.Server.ReadTimeout))
	}
	if time.Duration(cfg.Server.WriteTimeout) != 10*time.Second {
		t.Errorf("WriteTimeout = %v, want 10s", time.Duration(cfg.Server.WriteTimeout))
	}
	if cfg.Server.MaxRequestBytes != 2048 {
		t.Errorf("MaxRequestBytes = %d, want 2048", cfg.Server.MaxRequestBytes)
	}
	if cfg.Server.MaxResponseBytes != 4096 {
		t.Errorf("MaxResponseBytes = %d, want 4096", cfg.Server.MaxResponseBytes)
	}
}

func TestParseMultipleBackendsRequireDistinctPrefixes(t *testing.T) {
	yaml := `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "database-service"
    prefix: "db_"
    url: "http://db-primary.internal:9000/mcp"
  - id: "issue-tracker"
    prefix: "jira_"
    url: "http://jira.internal/mcp"
`
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("len(Backends) = %d, want 2", len(cfg.Backends))
	}
	if cfg.Backends[0].Prefix != "db_" || cfg.Backends[1].Prefix != "jira_" {
		t.Errorf("Backends = %+v, want prefixes db_ and jira_", cfg.Backends)
	}
}

func TestParsePolicies(t *testing.T) {
	yaml := `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: "db_read_*"
  - tool_pattern: "db_write_*"
`
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Policies) != 2 {
		t.Fatalf("len(Policies) = %d, want 2", len(cfg.Policies))
	}
	if cfg.Policies[0].ToolPattern != "db_read_*" || cfg.Policies[1].ToolPattern != "db_write_*" {
		t.Errorf("Policies = %+v, want tool_pattern db_read_* then db_write_*", cfg.Policies)
	}
}

func TestParsePolicyCircuitBreaker(t *testing.T) {
	yaml := `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: "db_write_*"
    resilience:
      circuit_breaker:
        failure_rate: 40.0
        window_duration: "30s"
        reset_timeout: "15s"
`
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cb := cfg.Policies[0].Resilience.CircuitBreaker
	if cb == nil {
		t.Fatal("Policies[0].Resilience.CircuitBreaker is nil, want it populated")
	}
	if cb.FailureRate != 40.0 {
		t.Errorf("FailureRate = %v, want 40.0", cb.FailureRate)
	}
	if time.Duration(cb.WindowDuration) != 30*time.Second {
		t.Errorf("WindowDuration = %v, want 30s", time.Duration(cb.WindowDuration))
	}
	if time.Duration(cb.ResetTimeout) != 15*time.Second {
		t.Errorf("ResetTimeout = %v, want 15s", time.Duration(cb.ResetTimeout))
	}
}

func TestParsePolicyRetries(t *testing.T) {
	yaml := `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: "db_read_*"
    resilience:
      retries:
        max_attempts: 3
        base_delay: "150ms"
        max_delay: "2000ms"
        backoff: "full_jitter"
`
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := cfg.Policies[0].Resilience.Retries
	if r == nil {
		t.Fatal("Policies[0].Resilience.Retries is nil, want it populated")
	}
	if r.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", r.MaxAttempts)
	}
	if time.Duration(r.BaseDelay) != 150*time.Millisecond {
		t.Errorf("BaseDelay = %v, want 150ms", time.Duration(r.BaseDelay))
	}
	if time.Duration(r.MaxDelay) != 2*time.Second {
		t.Errorf("MaxDelay = %v, want 2s", time.Duration(r.MaxDelay))
	}
	if r.Backoff != "full_jitter" {
		t.Errorf("Backoff = %q, want %q", r.Backoff, "full_jitter")
	}
}

func TestParsePolicyRetriesAppliesDefaults(t *testing.T) {
	yaml := `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: "db_read_*"
    resilience:
      retries: {}
`
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := cfg.Policies[0].Resilience.Retries
	if r == nil {
		t.Fatal("Policies[0].Resilience.Retries is nil, want it populated")
	}
	if r.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want default 5", r.MaxAttempts)
	}
	if time.Duration(r.BaseDelay) != 100*time.Millisecond {
		t.Errorf("BaseDelay = %v, want default 100ms", time.Duration(r.BaseDelay))
	}
	if time.Duration(r.MaxDelay) != 30*time.Second {
		t.Errorf("MaxDelay = %v, want default 30s", time.Duration(r.MaxDelay))
	}
	if r.Backoff != "full_jitter" {
		t.Errorf("Backoff = %q, want default %q", r.Backoff, "full_jitter")
	}
}

func TestParsePolicyRateLimit(t *testing.T) {
	yaml := `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: "db_read_*"
    resilience:
      rate_limit:
        rate: 100.0
        interval: "1s"
`
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rl := cfg.Policies[0].Resilience.RateLimit
	if rl == nil {
		t.Fatal("Policies[0].Resilience.RateLimit is nil, want it populated")
	}
	if rl.Rate != 100.0 {
		t.Errorf("Rate = %v, want 100.0", rl.Rate)
	}
	if time.Duration(rl.Interval) != time.Second {
		t.Errorf("Interval = %v, want 1s", time.Duration(rl.Interval))
	}
}

func TestParseNoPolicies(t *testing.T) {
	cfg, err := config.Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Policies) != 0 {
		t.Errorf("len(Policies) = %d, want 0 (policies: is optional)", len(cfg.Policies))
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "missing listen",
			yaml: `
server: {}
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
`,
			wantErr: "Server.Listen: is required",
		},
		{
			name: "unsupported server transport",
			yaml: `
server:
  listen: "0.0.0.0:8080"
  transport: "sse"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
`,
			wantErr: "Server.Transport: unsupported transport",
		},
		{
			name: "no backends",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends: []
`,
			wantErr: "Backends: is required",
		},
		{
			name: "two backends missing prefix",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "a"
    url: "http://a.internal/mcp"
  - id: "b"
    prefix: "b_"
    url: "http://b.internal/mcp"
`,
			wantErr: `backend "a": prefix is required when more than one backend is configured`,
		},
		{
			name: "two backends duplicate prefix",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "a"
    prefix: "svc_"
    url: "http://a.internal/mcp"
  - id: "b"
    prefix: "svc_"
    url: "http://b.internal/mcp"
`,
			wantErr: `duplicate prefix "svc_"`,
		},
		{
			name: "missing backend id",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - url: "http://svc.internal/mcp"
`,
			wantErr: "Backends[0].ID: is required",
		},
		{
			name: "missing backend url",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
`,
			wantErr: "Backends[0].URL: is required",
		},
		{
			name: "invalid backend url",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "not-a-url"
`,
			wantErr: "Backends[0].URL",
		},
		{
			name: "unsupported backend transport",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
    transport: "stdio"
`,
			wantErr: "Backends[0].Transport: unsupported transport",
		},
		{
			name: "negative read timeout",
			yaml: `
server:
  listen: "0.0.0.0:8080"
  read_timeout: "-5s"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
`,
			wantErr: "Server.ReadTimeout",
		},
		{
			name: "negative max request bytes",
			yaml: `
server:
  listen: "0.0.0.0:8080"
  max_request_bytes: -100
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
`,
			wantErr: "Server.MaxRequestBytes",
		},
		{
			name: "malformed duration",
			yaml: `
server:
  listen: "0.0.0.0:8080"
  read_timeout: "soon"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
`,
			wantErr: "invalid duration",
		},
		{
			name:    "malformed yaml",
			yaml:    "server: [this is not a map",
			wantErr: "parsing config",
		},
		{
			name: "missing policy tool_pattern",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: ""
`,
			wantErr: "Policies[0].ToolPattern: is required",
		},
		{
			name: "malformed policy glob",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: "db_[read_*"
`,
			wantErr: "invalid tool_pattern",
		},
		{
			name: "circuit breaker failure_rate out of range",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: "db_write_*"
    resilience:
      circuit_breaker:
        failure_rate: 150
`,
			wantErr: "circuit_breaker.failure_rate: must be between 0 and 100",
		},
		{
			name: "circuit breaker negative window_duration",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: "db_write_*"
    resilience:
      circuit_breaker:
        window_duration: "-1s"
`,
			wantErr: "circuit_breaker.window_duration: must not be negative",
		},
		{
			name: "retries negative base_delay",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: "db_read_*"
    resilience:
      retries:
        base_delay: "-150ms"
`,
			wantErr: "retries.base_delay: must not be negative",
		},
		{
			name: "retries unsupported backoff",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: "db_read_*"
    resilience:
      retries:
        backoff: "exponential"
`,
			wantErr: `retries.backoff: unsupported backoff "exponential"`,
		},
		{
			name: "rate limit missing rate",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: "db_read_*"
    resilience:
      rate_limit:
        interval: "1s"
`,
			wantErr: "rate_limit.rate: must be greater than 0",
		},
		{
			name: "rate limit missing interval",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "svc"
    url: "http://svc.internal/mcp"
policies:
  - tool_pattern: "db_read_*"
    resilience:
      rate_limit:
        rate: 100.0
`,
			wantErr: "rate_limit.interval: must be greater than 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("Parse: got nil error, want one containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Parse error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := config.Load("/nonexistent/mcp-resile.yaml")
	if err == nil {
		t.Fatal("Load: got nil error for a nonexistent file")
	}
}
