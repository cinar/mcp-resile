# Benchmarks

These numbers are copied directly from `go test -bench` output — nothing
here is estimated (spec.md §12, FEATURE-023). To reproduce:

```sh
go test ./internal/... -bench=. -benchmem -run '^$' -benchtime=3s
```

Measured on:

- Go 1.25.0, linux/amd64
- CPU: Intel(R) N100 (2 vCPUs visible to the benchmark process)
- Commit: `a8e97e7`

```
goos: linux
goarch: amd64
pkg: github.com/cinar/mcp-resile/internal/policy
cpu: Intel(R) N100
BenchmarkResolve-2   	18320412	       197.4 ns/op	       0 B/op	       0 allocs/op

pkg: github.com/cinar/mcp-resile/internal/proxy
cpu: Intel(R) N100
BenchmarkValidateArguments-2          	 2343924	      1620 ns/op	    1384 B/op	      26 allocs/op
BenchmarkCallToolNoPolicy-2           	    4977	    753115 ns/op	  808127 B/op	     655 allocs/op
BenchmarkCallToolWithFullPipeline-2   	    4845	    820253 ns/op	  808945 B/op	     673 allocs/op
```

## What's measured

The hot path a `tools/call` request takes through the gateway is: **validate
→ policy match → resile pipeline → dispatch**. Each stage has its own
benchmark, plus two end-to-end ones exercising the full chain against a real
backend:

- **`BenchmarkResolve`** (`internal/policy`) — the *policy match* stage:
  resolving a tool name against a five-policy list (four non-matching globs
  checked before the match), each call sharing the same `*Resolver` the real
  gateway would use for the lifetime of the process.
- **`BenchmarkValidateArguments`** (`internal/proxy`) — the *validate*
  stage: validating arguments against an already-compiled, cached
  `inputSchema` (the case every `tools/call` after a tool's first hits, per
  FEATURE-017's whole point — compiling the schema itself is a one-time cost
  outside the hot path).
- **`BenchmarkCallToolNoPolicy`** (`internal/proxy`) — the full chain for a
  tool matching no policy: schema validation, a policy-resolution miss, and
  dispatch through `resile.Do` with panic recovery only (FEATURE-016) — the
  floor cost every call pays. Runs against a real backend process
  (`net/http` + the real MCP Streamable HTTP transport on both legs, not a
  mock), so the number includes genuine network/serialization overhead, not
  just in-process CPU cost.
- **`BenchmarkCallToolWithFullPipeline`** (`internal/proxy`) — the same
  chain, but through a policy with a circuit breaker, retries, a rate
  limiter, and a min-deadline threshold all configured (FEATURE-011/012/
  013/015) and never triggered — isolating the resilience pipeline's own
  per-call bookkeeping overhead from the cost of an actual rejection or
  retry, which would dominate any such number and isn't representative of
  the common case.

`BenchmarkCallToolNoPolicy` vs. `BenchmarkCallToolWithFullPipeline` above
shows the full pipeline (circuit breaker check, rate limiter token
acquisition, retry-loop bookkeeping, min-deadline-threshold check) adding
roughly 9% over the no-policy floor — small next to the ~750µs a real HTTP
round trip to the backend costs either way, which dominates both numbers.

## What's not measured here

No SLA numbers are published from this. A benchmark against `httptest`'s
in-process backend on a 2-vCPU cloud instance says nothing about production
network latency, backend response size, or concurrent load — see spec.md
§9's own rationale for not publishing aspirational throughput/latency
claims.
