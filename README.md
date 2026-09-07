<p align="center">
    <a href="https://pkg.go.dev/github.com/cinar/mcp-resile"><img src="https://img.shields.io/badge/Go_Reference-007D9C?style=for-the-badge&logo=go&logoColor=white" alt="Go Reference" /></a>
    <a href="LICENSE"><img src="https://img.shields.io/github/license/cinar/mcp-resile?style=for-the-badge" alt="License" /></a>
    <a href="https://github.com/cinar/mcp-resile/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/cinar/mcp-resile/ci.yml?branch=main&style=for-the-badge&logo=github&label=CI" alt="Go CI" /></a>
    <a href="https://github.com/cinar/mcp-resile/stargazers"><img src="https://img.shields.io/github/stars/cinar/mcp-resile?style=for-the-badge&logo=github&logoColor=white" alt="GitHub Stars" /></a>
</p>

<h1 align="center">
    <img src="logo.svg" alt="mcp-resile logo: a shield around a circuit-breaker switch" width="128" height="128" /><br />
    mcp-resile
</h1>

<p align="center">mcp-resile is a resilience gateway for the Model Context Protocol that sits in front of your own MCP servers and protects them from autonomous-agent traffic, with zero backend code changes.</p>

<p align="center">
    <a href="#quick-start">Quick Start</a> &middot;
    <a href="#resilience-policies">Policies</a> &middot;
    <a href="#auth-metrics-and-logging">Auth &amp; Telemetry</a> &middot;
    <a href="#running-with-docker">Docker</a> &middot;
    <a href="#error-responses">Errors</a> &middot;
    <a href="BENCHMARKS.md">Benchmarks</a>
</p>

- **Circuit breaking** — a backend with a sustained failure rate gets taken out of rotation and fails fast, instead of every caller waiting out its own timeout.
- **Retries with full-jitter backoff** — only for transient failures (network errors, 502/503/504/429); an application-level error from the backend is never retried.
- **Token-bucket rate limiting** and **min-deadline-threshold** enforcement, per tool pattern.
- **Panic isolation** — a panic anywhere in a call's dispatch path is recovered and returned as a clean JSON-RPC error, never a crashed process.
- **JSON Schema validation** of `tools/call` arguments against each tool's own `inputSchema`, compiled and cached once per tool.
- **Response size clamping** — an oversized backend response is truncated with an LLM-readable notice, rather than blowing up the caller.
- **Bearer token / API key authentication** on every ingress request.
- **Prometheus metrics** (`/metrics`) and **structured `slog` logging** for every request, and config validation that fails loudly at startup instead of silently misbehaving.

All of it is driven by one YAML file, matched per tool by glob pattern — no code, no SDK to integrate into your backend.

## Table of Contents

- [Why mcp-resile?](#why-mcp-resile)
- [Quick Start](#quick-start)
- [Resilience Policies](#resilience-policies)
- [Auth, Metrics, and Logging](#auth-metrics-and-logging)
- [Running with Docker](#running-with-docker)
- [Configuration Reference](#configuration-reference)
- [Error Responses](#error-responses)
- [Development](#development)
- [Contributing to the Project](#contributing-to-the-project)
- [License](#license)

## Why mcp-resile?

MCP is how AI agents call tools, but an agent is a different kind of client than a human-built app: it can retry aggressively when confused, send malformed or oversized requests, and hammer a slow endpoint without backing off. There's no standard for auth or rate limiting in a typical hand-rolled MCP server, so every team that stands one up ends up either building the same defensive plumbing themselves, or not — until an agent takes their backend down.

mcp-resile is a **reverse proxy**, not a library: your backend doesn't change at all. Agents talk to mcp-resile instead of talking to your server directly, and it forwards calls through, layering resilience and safety controls on top:

1. **Zero reinvention.** All resilience logic (circuit breaker, retries, rate limiting) comes from [`cinar/resile`](https://github.com/cinar/resile). All MCP protocol handling comes from [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk). mcp-resile's own code is routing, a schema firewall, output guardrails, and the glue between the two — not a reimplementation of either.
2. **Declarative, not code.** Every policy is YAML matched by glob pattern against a tool name. There's no SDK to import into your backend and no code path to keep in sync with it.
3. **Honest about what's measured.** [BENCHMARKS.md](BENCHMARKS.md) publishes real `go test -bench` output, not estimated numbers, and this README doesn't claim a resilience feature works before it's been tested against a real backend process.

**Out of scope for v1** (tracked, not forgotten): egress sidecar mode, SSE/stdio transports, speculative hedging, adaptive concurrency, priority bulkheads, full RBAC/OAuth 2.1, and a semantic idempotency cache.

## Quick Start

You'll need Go 1.24+ and an MCP server of your own reachable over Streamable HTTP. This gets a gateway running in front of it in a couple of minutes.

**1. Build**

```sh
git clone https://github.com/cinar/mcp-resile.git
cd mcp-resile
make build           # -> bin/mcp-resile (CGO_ENABLED=0, static binary)
```

**2. Write a config**

Create `mcp-resile.yaml`, pointing `backends[0].url` at your own server:

```yaml
version: "v1"

server:
  listen: "0.0.0.0:8080"

backends:
  - id: "my-service"
    url: "http://127.0.0.1:9999/mcp"   # <- your MCP server's Streamable HTTP endpoint
```

**3. Run it**

```sh
./bin/mcp-resile --config mcp-resile.yaml
# mcp-resile v1.0.0 listening on 0.0.0.0:8080, proxying to 1 backend(s)
```

**4. Point a client at the gateway instead of your backend**

Any Streamable HTTP MCP client works unchanged — mcp-resile is a transparent proxy at the protocol level. Swap your client's server URL from `http://127.0.0.1:9999/mcp` to `http://127.0.0.1:8080`, and every `tools/list`/`tools/call` now goes through the resilience pipeline before reaching your backend.

## Resilience Policies

Policies match tools by glob pattern against `tool_pattern` (first match in the list wins) and layer any combination of circuit breaker, retries, rate limiting, and a minimum deadline threshold onto matching calls:

```yaml
policies:
  - tool_pattern: "db_write_*"
    resilience:
      circuit_breaker:
        failure_rate: 40.0       # percent
        window_duration: "30s"
        reset_timeout: "15s"
      retries:
        max_attempts: 3
        base_delay: "150ms"
        max_delay: "2000ms"
        backoff: "full_jitter"   # the only backoff v1 supports
      rate_limit:
        rate: 100.0
        interval: "1s"
      min_deadline_threshold: "50ms"
```

A tool matching no policy still gets schema validation, response clamping, and panic recovery — those apply gateway-wide, not per policy.

## Auth, Metrics, and Logging

```yaml
auth:
  enabled: true
  header: "Authorization"        # or e.g. "X-API-Key"
  tokens: ["shared-secret-key-1"]

telemetry:
  metrics:
    enabled: true
    port: 9090
    path: "/metrics"
  logging:
    level: "info"                # debug, info, warn, error
    format: "json"                # json, text
```

## Running with Docker

A prebuilt image is published to GitHub Container Registry on every `vX.Y.Z` tag:

```sh
docker run -p 8080:8080 \
  -v "$(pwd)/mcp-resile.yaml:/etc/mcp-resile/mcp-resile.yaml:ro" \
  ghcr.io/cinar/mcp-resile:latest
```

Or build it yourself:

```sh
docker build -t mcp-resile .
docker run -p 8080:8080 \
  -v "$(pwd)/mcp-resile.yaml:/etc/mcp-resile/mcp-resile.yaml:ro" \
  mcp-resile
```

The image is built on `gcr.io/distroless/static:nonroot` — no shell, no package manager, runs as a non-root user — and published for both `linux/amd64` and `linux/arm64`. Pass `--build-arg VERSION=$(git describe --tags --always --dirty)` to stamp a real version into a locally built image; it defaults to `dev`.

## Configuration Reference

| Section | Field | Meaning |
| :--- | :--- | :--- |
| `server` | `listen`, `read_timeout`, `write_timeout`, `max_request_bytes`, `max_response_bytes` | Ingress HTTP server settings. `transport` is always `"streamable-http"` in v1. |
| `auth` | `enabled`, `header`, `tokens` | Bearer/API-key allow-list; disabled by default. |
| `backends[]` | `id`, `prefix`, `url` | One entry per MCP server. `prefix` namespaces tool names (`query` → `db_query`) and is required once you have more than one backend. |
| `policies[]` | `tool_pattern`, `resilience.*` | Glob-matched resilience config; see [Resilience Policies](#resilience-policies). |
| `telemetry.metrics` | `enabled`, `port`, `path` | Prometheus endpoint, served on its own port. |
| `telemetry.logging` | `level`, `format` | Structured `slog` output. |

An invalid config (a typo'd field, a malformed glob, a duplicate backend id or prefix) fails at startup with a message naming the offending field and the config file's path — never a generic parse error.

## Error Responses

A rejected call comes back as a normal JSON-RPC error your agent can act on, not a bare connection failure:

| Failure | Code | Notes |
| :--- | :--- | :--- |
| Schema validation | `-32602` | Field-level detail so the caller can self-correct. |
| Unauthorized | `-32601` | Deliberately indistinguishable from an unknown method. |
| Rate limit exceeded | `-33001` | Includes `retry_after_ms`. |
| Circuit open | `-33002` | Backend is being protected; stop retrying immediately. |
| Execution timeout | `-33003` | Deadline exceeded before or during dispatch. |
| Response truncated | *(success)* | `isError: false`; a `[TRUNCATED: ...]` notice is appended. |

## Development

```sh
make build   # CGO_ENABLED=0 go build -o bin/mcp-resile ./cmd/mcp-resile
make test    # go test -race ./...
make lint    # go vet, plus golangci-lint if installed
```

See [BENCHMARKS.md](BENCHMARKS.md) for measured (not estimated) performance numbers for the hot path.

[![mcp-resile MCP server — quality and maintenance score on Glama](https://glama.ai/mcp/servers/cinar/mcp-resile/badges/score.svg)](https://glama.ai/mcp/servers/cinar/mcp-resile)

## Contributing to the Project

Issues and pull requests are welcome. If you're proposing a larger change, please open an issue first so the approach can be discussed before you put in the work.

## License

mcp-resile is provided under the MIT License, reproduced below and also available in the [LICENSE](./LICENSE) file.

```
MIT License

Copyright (c) 2026 Onur Cinar

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
