# mcp-resile

A resilience gateway/reverse proxy for the [Model Context
Protocol](https://modelcontextprotocol.io) (MCP). Sits in front of your own
MCP servers and protects them from autonomous-agent traffic — retry storms,
oversized payloads, malformed input — **without any changes to the
backends**.

All resilience logic (circuit breaker, jittered retries, rate limiting,
panic recovery) comes from [`cinar/resile`](https://github.com/cinar/resile).
All MCP protocol handling comes from
[`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk).
mcp-resile itself is routing, a schema firewall, output guardrails, and the
glue between the two.

## What v1 does

Point mcp-resile at one or more of your own MCP servers (Streamable HTTP)
and it gives every one of them, with zero backend code changes:

- **Circuit breaking** — a backend with a sustained failure rate gets taken
  out of rotation and fails fast, instead of every caller waiting out its
  own timeout.
- **Retries with full-jitter backoff** — only for transient failures
  (network errors, 502/503/504/429); an application-level error from the
  backend is never retried.
- **Token-bucket rate limiting** and **min-deadline-threshold** enforcement,
  per tool pattern.
- **Panic isolation** — a panic anywhere in a call's dispatch path is
  recovered and returned as a clean JSON-RPC error, never a crashed process.
- **JSON Schema validation** of `tools/call` arguments against each tool's
  own `inputSchema`, compiled and cached once per tool.
- **Response size clamping** — an oversized backend response is truncated
  with an LLM-readable notice, rather than blowing up the caller.
- **Bearer token / API key authentication** on every ingress request.
- **Prometheus metrics** (`/metrics`) and **structured `slog` logging** for
  every request.

All of it is driven by one YAML file, matched per tool by glob pattern — no
code, no SDK to integrate into your backend.

**Out of scope for v1** (tracked, not forgotten): egress sidecar mode, SSE/
stdio transports, speculative hedging, adaptive concurrency, priority
bulkheads, full RBAC/OAuth 2.1, and a semantic idempotency cache.

## Quick start

You'll need Go 1.24+ and an MCP server of your own reachable over
Streamable HTTP. This gets a gateway running in front of it in a couple of
minutes.

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

Any Streamable HTTP MCP client works unchanged — mcp-resile is a
transparent proxy at the protocol level. Swap your client's server URL from
`http://127.0.0.1:9999/mcp` to `http://127.0.0.1:8080`, and every
`tools/list`/`tools/call` now goes through the resilience pipeline before
reaching your backend.

### Adding resilience policies

Policies match tools by glob pattern against `tool_pattern` (first match in
the list wins) and layer any combination of circuit breaker, retries, rate
limiting, and a minimum deadline threshold onto matching calls:

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

A tool matching no policy still gets schema validation, response clamping,
and panic recovery — those apply gateway-wide, not per policy.

### Auth, metrics, and logging

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

### Running with Docker

```sh
docker build -t mcp-resile .
docker run -p 8080:8080 \
  -v "$(pwd)/mcp-resile.yaml:/etc/mcp-resile/mcp-resile.yaml:ro" \
  mcp-resile
```

The image is built on `gcr.io/distroless/static:nonroot` — no shell, no
package manager, runs as a non-root user. Pass
`--build-arg VERSION=$(git describe --tags --always --dirty)` to stamp a
real version into the binary; it defaults to `dev`.

## Configuration reference

| Section | Field | Meaning |
| :--- | :--- | :--- |
| `server` | `listen`, `read_timeout`, `write_timeout`, `max_request_bytes`, `max_response_bytes` | Ingress HTTP server settings. `transport` is always `"streamable-http"` in v1. |
| `auth` | `enabled`, `header`, `tokens` | Bearer/API-key allow-list; disabled by default. |
| `backends[]` | `id`, `prefix`, `url` | One entry per MCP server. `prefix` namespaces tool names (`query` → `db_query`) and is required once you have more than one backend. |
| `policies[]` | `tool_pattern`, `resilience.*` | Glob-matched resilience config; see above. |
| `telemetry.metrics` | `enabled`, `port`, `path` | Prometheus endpoint, served on its own port. |
| `telemetry.logging` | `level`, `format` | Structured `slog` output. |

An invalid config (a typo'd field, a malformed glob, a duplicate backend id
or prefix) fails at startup with a message naming the offending field and
the config file's path — never a generic parse error.

## Error responses

A rejected call comes back as a normal JSON-RPC error your agent can act
on, not a bare connection failure:

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

See [BENCHMARKS.md](BENCHMARKS.md) for measured (not estimated) performance
numbers for the hot path.

## License

[MIT](LICENSE)
