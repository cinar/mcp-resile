// Package proxy wires the ingress MCP server to one or more egress
// backends, merging tools/list across them and routing tools/call by
// namespace prefix, per spec.md §5.1, and dispatching each tools/call
// through resile's resilience primitives per the policy governing it
// (FEATURE-010 onward). Schema firewall and output guardrail logic lives
// elsewhere, added by their own features.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/cinar/resile"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cinar/mcp-resile/internal/config"
	"github.com/cinar/mcp-resile/internal/egress"
	"github.com/cinar/mcp-resile/internal/metrics"
	"github.com/cinar/mcp-resile/internal/policy"
)

// V1 forwards exactly these two methods; everything else (initialize,
// ping, notifications, ...) is handled by the SDK itself.
const (
	methodListTools = "tools/list"
	methodCallTool  = "tools/call"
)

// Route associates one egress backend with the namespace prefix used to
// disambiguate its tools from every other backend's. Prefix may be empty
// only when it is the sole route (config.Load enforces this). ID is the
// backend's config.Backend.ID, carried through purely for structured
// logging (FEATURE-021) — egress.Backend itself exposes no identifier of
// its own.
type Route struct {
	ID      string
	Prefix  string
	Backend *egress.Backend
}

// Middleware returns an mcp.Middleware that merges tools/list across all
// routes, renaming each tool with its backend's prefix, and routes
// tools/call to the backend whose prefix matches the requested tool name
// (longest prefix wins), stripping the prefix before forwarding. A
// tools/call for a name that matches no route falls through to next, which
// reports it as an unknown tool exactly as a single, un-proxied server
// would. router tracks progress tokens across the call so a later
// notifications/progress from the backend can be routed back to the right
// client (FEATURE-009); notifications/cancelled needs no handling here,
// since it propagates automatically through ctx (see NotificationRouter's
// doc comment). policies resolves the (unprefixed) tool name to the policy
// governing its dispatch, if any (FEATURE-010 onward). Every tools/call is
// also validated against its tool's own inputSchema before dispatch
// (FEATURE-017), via a schema cache private to this Middleware call, and a
// successful response's text content is clamped to maxResponseBytes,
// truncated with a notice appended if it doesn't fit (FEATURE-018).
func Middleware(routes []Route, router *NotificationRouter, policies *policy.Resolver, maxResponseBytes int64, m *metrics.Metrics, logger *slog.Logger) mcp.Middleware {
	schemas := newSchemaCache()

	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case methodListTools:
				return listTools(ctx, routes, schemas)
			case methodCallTool:
				return callTool(ctx, routes, req, next, router, policies, schemas, maxResponseBytes, m, logger)
			default:
				return next(ctx, method, req)
			}
		}
	}
}

// listTools calls tools/list on every route and merges the results, with
// each returned tool renamed to include its backend's prefix. It also
// primes schemas with every tool's compiled inputSchema, so a tools/call
// that follows a client's own tools/list never pays a schema-compilation
// round trip (FEATURE-017); a tools/call for a tool nothing has listed yet
// still resolves and caches its schema lazily, see schemaCache.resolveTool.
// A tool whose inputSchema fails to compile is still listed — one backend's
// malformed schema shouldn't hide its tools or anyone else's; that tool's
// calls simply fall back to schemaCache.resolveTool's own miss handling
// when dispatched (see callTool).
func listTools(ctx context.Context, routes []Route, schemas *schemaCache) (*mcp.ListToolsResult, error) {
	merged := &mcp.ListToolsResult{}

	for _, route := range routes {
		result, err := route.Backend.ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing tools for prefix %q: %w", route.Prefix, err)
		}

		for _, tool := range result.Tools {
			prefixed := *tool
			prefixed.Name = route.Prefix + tool.Name
			merged.Tools = append(merged.Tools, &prefixed)
			_ = schemas.refresh(prefixed.Name, &prefixed)
		}
	}

	return merged, nil
}

// callTool routes a tools/call request to the matching backend, or falls
// through to next if no route matches. If the request carries a progress
// token, it's swapped for one that identifies this call uniquely to router
// before forwarding, so a resulting notifications/progress from the backend
// can be routed back to the calling client and restored to its own token.
// Every dispatch to a backend — matched by a policy or not — runs through
// resile.Do with at least panic recovery enabled (FEATURE-016), so a panic
// inside the backend call or its response deserializer never crashes the
// gateway process; it comes back as a clean internal error instead. The
// (unprefixed) tool name is also resolved against policies; a match with a
// circuit breaker, retries, a rate limit, and/or a min-deadline threshold
// configured folds those into the same resile.Do call (FEATURE-011,
// FEATURE-012, FEATURE-013, FEATURE-015), so sustained backend failures
// make subsequent calls fail fast, transient ones get retried with
// full-jitter backoff, excess calls are shed before reaching the backend,
// and a request too close to its deadline is aborted before a wasted round
// trip. Any resulting circuit-open, rate-limit, timeout, or panic error is
// mapped to its documented JSON-RPC code (FEATURE-014) rather than reaching
// the client as a generic internal error. Before any of that, the request's
// arguments are validated against the tool's own inputSchema, if one is
// known (FEATURE-017); a violation returns -32602 without ever reaching
// resile.Do, so it isn't counted as a circuit-breaker failure or charged
// against the rate limit. Schema *resolution* failing — the tool declares
// no schema, its schema doesn't compile, or fetching it from the backend
// errors — is not itself a violation: dispatch proceeds unvalidated and
// lets the normal resilience pipeline handle a genuinely unreachable
// backend, rather than a validation-layer error masking it. A successful
// response's text content is then clamped to maxResponseBytes, truncated
// with a notice appended if it doesn't fit (FEATURE-018); this happens
// after dispatch, unconditionally, regardless of policy match.
func callTool(ctx context.Context, routes []Route, req mcp.Request, next mcp.MethodHandler, router *NotificationRouter, policies *policy.Resolver, schemas *schemaCache, maxResponseBytes int64, m *metrics.Metrics, logger *slog.Logger) (mcp.Result, error) {
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok {
		return nil, fmt.Errorf("proxy: unexpected params type %T for %s", req.GetParams(), methodCallTool)
	}

	route, name, ok := matchRoute(routes, params.Name)
	if !ok {
		return next(ctx, methodCallTool, req)
	}

	start := time.Now()
	result, err := dispatchTool(ctx, route, name, params, req, router, policies, schemas, maxResponseBytes, m)
	duration := time.Since(start)
	outcome := outcomeForError(err)
	m.RecordRequest(params.Name, outcome, duration)
	logDispatch(ctx, logger, params.Name, route.ID, outcome, duration, err)
	return result, err
}

// logDispatch logs one tools/call dispatch's outcome, per the FEATURE-021
// acceptance criterion: a successful call logs at info with tool name,
// backend, outcome, and latency; a failed one logs at warn (or error, for a
// recovered panic — the one outcome that indicates a bug rather than an
// expected rejection) with the same fields plus the mapped JSON-RPC error
// code, when the error carries one.
func logDispatch(ctx context.Context, logger *slog.Logger, tool, backend, outcome string, duration time.Duration, err error) {
	attrs := []slog.Attr{
		slog.String("tool", tool),
		slog.String("backend", backend),
		slog.String("outcome", outcome),
		slog.Duration("latency", duration),
	}

	if err == nil {
		logger.LogAttrs(ctx, slog.LevelInfo, "tools/call", attrs...)
		return
	}

	level := slog.LevelWarn
	if outcome == outcomeInternalError {
		level = slog.LevelError
	}
	if code, ok := jsonrpcCode(err); ok {
		attrs = append(attrs, slog.Int64("code", code))
	}
	logger.LogAttrs(ctx, level, "tools/call", attrs...)
}

// dispatchTool does the actual validate-then-dispatch work for one matched
// tools/call, once callTool has already resolved which route it belongs to.
// Split out from callTool so callTool's single RecordRequest call at the end
// covers every return path here uniformly, including the schema-validation
// rejection.
func dispatchTool(ctx context.Context, route Route, name string, params *mcp.CallToolParamsRaw, req mcp.Request, router *NotificationRouter, policies *policy.Resolver, schemas *schemaCache, maxResponseBytes int64, m *metrics.Metrics) (mcp.Result, error) {
	forwarded := &mcp.CallToolParams{
		Meta:      maps.Clone(params.Meta),
		Name:      name,
		Arguments: json.RawMessage(params.Arguments),
	}

	if resolved, ok, err := schemas.resolveTool(ctx, route, params.Name); err == nil && ok {
		if err := validateArguments(resolved, name, params.Arguments); err != nil {
			return nil, err
		}
	}

	if session, ok := req.GetSession().(*mcp.ServerSession); ok {
		token, done := router.TrackProgress(session, params.GetProgressToken())
		defer done()
		if token != "" {
			forwarded.SetProgressToken(token)
		}
	}

	dispatch := func(ctx context.Context) (*mcp.CallToolResult, error) {
		return route.Backend.CallTool(ctx, forwarded)
	}

	p, matched := policies.Resolve(name)

	opts := baseResilienceOptions(params.Name, m)
	if matched {
		opts = resilienceOptions(p, params.Name, m)
	}

	result, err := resile.Do(ctx, dispatch, opts...)
	if err != nil {
		var rateLimit *config.RateLimit
		if matched {
			rateLimit = p.Resilience.RateLimit
		}
		return nil, mapResilienceError(err, rateLimit)
	}
	return clampResponse(result, maxResponseBytes), nil
}

// baseResilienceOptions is what a tool name matching no policy dispatches
// with: panic recovery only, one attempt, no deadline threshold, plus
// metrics instrumentation. It's what resilienceOptions itself also starts
// from — kept as its own function so both paths build the same baseline the
// same way. name is the full (prefixed) tool name, used to label the
// resulting metrics (FEATURE-020) via resile.WithName/RetryState.Name.
func baseResilienceOptions(name string, m *metrics.Metrics) []resile.Option {
	return []resile.Option{
		resile.WithName(name),
		resile.WithInstrumenter(m.Instrumenter()),
		resile.WithPanicRecovery(),
		resile.WithMaxAttempts(1),
		resile.WithMinDeadlineThreshold(0),
	}
}

// resilienceOptions builds the resile.Options a policy's dispatch should run
// with. A circuit breaker, retries, a rate limit, and a min-deadline
// threshold can all be configured on the same policy and combine into one
// resile.Do call, matching spec.md §8's ToolExecutionEngine example.
//
// WithMinDeadlineThreshold is always passed, even when
// resilience.min_deadline_threshold: is unset (Duration's zero value):
// every resile.Do call starts from resile.DefaultConfig(), which sets its
// own 5ms threshold, so leaving the option off entirely would silently opt
// an unconfigured policy into that default the moment a caller's ctx
// happens to carry a deadline. Threshold 0 only aborts a request whose
// deadline has already passed, which is what "unconfigured" should mean.
func resilienceOptions(p *policy.Policy, name string, m *metrics.Metrics) []resile.Option {
	opts := []resile.Option{
		resile.WithName(name),
		resile.WithInstrumenter(m.Instrumenter()),
		resile.WithPanicRecovery(),
		resile.WithMinDeadlineThreshold(time.Duration(p.Resilience.MinDeadlineThreshold)),
	}

	if cb := p.CircuitBreaker(); cb != nil {
		opts = append(opts, resile.WithCircuitBreaker(cb))
	}

	if rl := p.RateLimiter(); rl != nil {
		opts = append(opts, resile.WithRateLimiterInstance(rl))
	}

	if r := p.Resilience.Retries; r != nil {
		opts = append(opts,
			resile.WithMaxAttempts(r.MaxAttempts),
			resile.WithBackoff(resile.NewFullJitter(time.Duration(r.BaseDelay), time.Duration(r.MaxDelay))),
			resile.WithRetryIfFunc(isTransientError),
		)
	} else {
		// No retries configured: pin attempts to 1. resile.Do otherwise
		// defaults to 5 attempts with full-jitter backoff
		// (resile.DefaultConfig), which would silently retry on this
		// policy's behalf without it having asked for that.
		opts = append(opts, resile.WithMaxAttempts(1))
	}

	return opts
}

// MergeCapabilities returns the union of every route's backend capabilities
// (tools, resources, prompts), per spec.md §5.1: a capability is present if
// any backend advertises it, and its ListChanged/Subscribe flags are true if
// any backend sets them. The gateway itself never registers tools,
// resources, or prompts on its own SDK server (routing is done via
// middleware instead), so without this its initialize response would
// advertise none of them regardless of what the backends actually offer.
func MergeCapabilities(routes []Route) *mcp.ServerCapabilities {
	merged := &mcp.ServerCapabilities{Logging: &mcp.LoggingCapabilities{}}

	for _, route := range routes {
		caps := route.Backend.Capabilities()
		if caps == nil {
			continue
		}

		if caps.Tools != nil {
			if merged.Tools == nil {
				merged.Tools = &mcp.ToolCapabilities{}
			}
			merged.Tools.ListChanged = merged.Tools.ListChanged || caps.Tools.ListChanged
		}

		if caps.Resources != nil {
			if merged.Resources == nil {
				merged.Resources = &mcp.ResourceCapabilities{}
			}
			merged.Resources.ListChanged = merged.Resources.ListChanged || caps.Resources.ListChanged
			merged.Resources.Subscribe = merged.Resources.Subscribe || caps.Resources.Subscribe
		}

		if caps.Prompts != nil {
			if merged.Prompts == nil {
				merged.Prompts = &mcp.PromptCapabilities{}
			}
			merged.Prompts.ListChanged = merged.Prompts.ListChanged || caps.Prompts.ListChanged
		}
	}

	return merged
}

// matchRoute finds the route whose prefix matches toolName, preferring the
// longest prefix so a more specific prefix always wins over a shorter (or
// empty, catch-all) one. It returns the tool name with that prefix
// stripped, ready to forward to the backend.
func matchRoute(routes []Route, toolName string) (route Route, name string, ok bool) {
	bestLen := -1

	for _, r := range routes {
		if strings.HasPrefix(toolName, r.Prefix) && len(r.Prefix) > bestLen {
			route, bestLen = r, len(r.Prefix)
		}
	}

	if bestLen < 0 {
		return Route{}, "", false
	}

	return route, toolName[bestLen:], true
}
