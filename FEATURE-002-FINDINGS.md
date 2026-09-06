# FEATURE-002: SDK dual-role spike — findings

Spike code lived in `spike/` (its own Go module, deleted after this writeup
per the acceptance criteria). It wired:

- a real backend `mcp.Server` with one tool (`echo`),
- a gateway `mcp.Client` session connected to that backend,
- a gateway `mcp.Server` toward a test `mcp.Client`, using `go-sdk`'s
  in-memory transport pair on both hops.

All three ran in one process. The test client called `tools/list` and
`tools/call` through the gateway and got real backend results back.

## The hook point: `Server.AddReceivingMiddleware`

`go-sdk` already has the seam FEATURE-002 needed:

```go
type MethodHandler func(ctx context.Context, method string, req Request) (result Result, err error)
type Middleware func(MethodHandler) MethodHandler

server.AddReceivingMiddleware(func(next MethodHandler) MethodHandler {
    return func(ctx context.Context, method string, req Request) (Result, error) {
        if method == "tools/call" {
            // inspect/modify, then either call next(...) or short-circuit
            // and forward to the backend client session ourselves.
        }
        return next(ctx, method, req)
    }
})
```

This sees **every** incoming JSON-RPC method on the gateway's server side —
`initialize`, `ping`, `tools/list`, `tools/call`, notifications — with
params already type-switched (`req.GetParams()`). It's the single
integration point for routing, schema firewall, and (later) wrapping the
whole dispatch in a `resile` policy. No need to hand-register per-tool
handlers on the gateway side at all: `tools/list`/`tools/call` are simply
intercepted and forwarded, which is what lets the gateway proxy backend
tools it has never seen at compile time.

`Client.AddReceivingMiddleware` / `AddSendingMiddleware` exist too (for the
gateway's outbound leg toward backends), if resilience policies ever need
to wrap the client side instead of the server side.

## `CallToolParamsRaw` gives raw arguments for free

Server-side `tools/call` params arrive as `*CallToolParamsRaw`, whose
`Arguments` field is `json.RawMessage` — not yet unmarshaled. That's exactly
what a passthrough gateway wants: it can forward bytes to the backend
without knowing every tool's argument shape, and it's the natural place for
FEATURE-017's schema validation to run (validate the raw bytes against the
cached compiled schema before dispatch).

## Pitfall: don't stamp gateway metadata into `Arguments`

First attempt at "modify the request in flight" added a marker key
(`_seen_by_gateway`) directly into the decoded `Arguments` map before
forwarding. The backend's generated input schema has
`"additionalProperties": false` (the default for struct-based schemas via
`AddTool`), so the backend rejected the call:

```
validating "arguments": validating root: unexpected additional properties ["_seen_by_gateway"]
```

**Implication for later features:** any gateway-attached metadata
(resilience state, trace IDs, policy decisions) must travel in
`CallToolParams.Meta` (`map[string]any`, wire field `_meta`), never spliced
into `Arguments`. Arguments should be forwarded byte-for-byte except when
FEATURE-017's schema validation itself needs to reject or (rarely) coerce
them.

## Capability/tool merging isn't automatic

The gateway server has no tools of its own — `tools/list` only returns
something because the middleware explicitly forwards it to the backend
session. With multiple backends (FEATURE-007), the middleware will need to
fan out `tools/list` to every connected backend, merge + namespace-prefix
the results, and route `tools/call` by prefix. `initialize` capability
merging (FEATURE-008) is the same shape: nothing merges for free, the
middleware does it.

## Recommendation

Proceed with `AddReceivingMiddleware` on the gateway's `mcp.Server` as the
single hook point for routing (FEATURE-007), the policy engine
(FEATURE-010), and the resile/error-mapping wrapper (FEATURE-011–014): one
middleware, dispatch by `method`, `tools/call` gets the full
policy-match → resile-pipeline → dispatch treatment, everything else passes
through to `next`. No workarounds needed against the SDK; go straight into
FEATURE-003/004/005/006.
