// Package proxy wires the ingress MCP server to one or more egress
// backends, merging tools/list across them and routing tools/call by
// namespace prefix, per spec.md §5.1. No resilience or firewall logic
// lives here yet, that arrives with FEATURE-010 onward.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cinar/mcp-resile/internal/egress"
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
// would.
func Middleware(routes []Route) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case methodListTools:
				return listTools(ctx, routes)
			case methodCallTool:
				return callTool(ctx, routes, req, next)
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
// through to next if no route matches.
func callTool(ctx context.Context, routes []Route, req mcp.Request, next mcp.MethodHandler) (mcp.Result, error) {
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok {
		return nil, fmt.Errorf("proxy: unexpected params type %T for %s", req.GetParams(), methodCallTool)
	}

	route, name, ok := matchRoute(routes, params.Name)
	if !ok {
		return next(ctx, methodCallTool, req)
	}

	return route.Backend.CallTool(ctx, &mcp.CallToolParams{
		Meta:      params.Meta,
		Name:      name,
		Arguments: json.RawMessage(params.Arguments),
	})
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
