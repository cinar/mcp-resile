// Package proxy wires the ingress MCP server to one or more egress
// backends, merging tools/list across them and routing tools/call by
// namespace prefix, per spec.md §5.1. No resilience or firewall logic
// lives here yet, that arrives with FEATURE-010 onward.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/cinar/resile"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cinar/mcp-resile/internal/egress"
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
// only when it is the sole route (config.Load enforces this).
type Route struct {
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
// governing its dispatch, if any (FEATURE-010 onward).
func Middleware(routes []Route, router *NotificationRouter, policies *policy.Resolver) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case methodListTools:
				return listTools(ctx, routes)
			case methodCallTool:
				return callTool(ctx, routes, req, next, router, policies)
			default:
				return next(ctx, method, req)
			}
		}
	}
}

// listTools calls tools/list on every route and merges the results, with
// each returned tool renamed to include its backend's prefix.
func listTools(ctx context.Context, routes []Route) (*mcp.ListToolsResult, error) {
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
		}
	}

	return merged, nil
}

// callTool routes a tools/call request to the matching backend, or falls
// through to next if no route matches. If the request carries a progress
// token, it's swapped for one that identifies this call uniquely to router
// before forwarding, so a resulting notifications/progress from the backend
// can be routed back to the calling client and restored to its own token.
// The (unprefixed) tool name is resolved against policies; a match with a
// circuit breaker and/or retries configured dispatches through resile
// (FEATURE-011, FEATURE-012), so sustained backend failures make subsequent
// calls fail fast, and transient ones get retried with full-jitter backoff,
// instead of every failure reaching the client as-is.
func callTool(ctx context.Context, routes []Route, req mcp.Request, next mcp.MethodHandler, router *NotificationRouter, policies *policy.Resolver) (mcp.Result, error) {
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok {
		return nil, fmt.Errorf("proxy: unexpected params type %T for %s", req.GetParams(), methodCallTool)
	}

	route, name, ok := matchRoute(routes, params.Name)
	if !ok {
		return next(ctx, methodCallTool, req)
	}

	forwarded := &mcp.CallToolParams{
		Meta:      maps.Clone(params.Meta),
		Name:      name,
		Arguments: json.RawMessage(params.Arguments),
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

	p, ok := policies.Resolve(name)
	if !ok {
		return dispatch(ctx)
	}

	opts := resilienceOptions(p)
	if len(opts) == 0 {
		// No circuit breaker or retries configured for this policy: dispatch
		// directly rather than through an "empty" resile.Do, which would
		// otherwise silently inherit resile.DefaultConfig's own defaults
		// (e.g. a 5ms MinDeadlineThreshold) ahead of FEATURE-015 wiring it
		// up on purpose.
		return dispatch(ctx)
	}

	return resile.Do(ctx, dispatch, opts...)
}

// resilienceOptions builds the resile.Options a policy's dispatch should run
// with. A circuit breaker and retries can both be configured on the same
// policy and combine into one resile.Do call, matching spec.md §8's
// ToolExecutionEngine example.
func resilienceOptions(p *policy.Policy) []resile.Option {
	var opts []resile.Option

	if cb := p.CircuitBreaker(); cb != nil {
		opts = append(opts, resile.WithCircuitBreaker(cb))
	}

	if r := p.Resilience.Retries; r != nil {
		opts = append(opts,
			resile.WithMaxAttempts(r.MaxAttempts),
			resile.WithBackoff(resile.NewFullJitter(time.Duration(r.BaseDelay), time.Duration(r.MaxDelay))),
			resile.WithRetryIfFunc(isTransientError),
		)
	} else if len(opts) > 0 {
		// A circuit breaker but no retries: pin attempts to 1. resile.Do
		// otherwise defaults to 5 attempts with full-jitter backoff
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
