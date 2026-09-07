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
			wantErr: "v1 supports exactly one backend, got 0",
		},
		{
			name: "two backends",
			yaml: `
server:
  listen: "0.0.0.0:8080"
backends:
  - id: "a"
    url: "http://a.internal/mcp"
  - id: "b"
    url: "http://b.internal/mcp"
`,
			wantErr: "v1 supports exactly one backend, got 2",
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
