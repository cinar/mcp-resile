// Package proxy wires the ingress MCP server to a single egress backend,
// forwarding tools/list and tools/call unmodified. It is the walking
// skeleton from spec.md §12.1 FEATURE-006: no resilience or firewall logic
// lives here yet, that arrives with FEATURE-010 onward.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cinar/mcp-resile/internal/egress"
)

// V1 forwards exactly these two methods; everything else (initialize,
// ping, notifications, ...) is handled by the SDK itself.
const (
	methodListTools = "tools/list"
	methodCallTool  = "tools/call"
)

// Middleware returns an mcp.Middleware that forwards tools/list and
// tools/call to backend, byte-for-byte, and passes every other method
// through to the next handler in the chain.
func Middleware(backend *egress.Backend) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case methodListTools:
				return backend.ListTools(ctx)
			case methodCallTool:
				return callTool(ctx, backend, req)
			default:
				return next(ctx, method, req)
			}
		}
	}
}

// callTool forwards a tools/call request to backend. Arguments are
// forwarded as the raw bytes received from the client (never decoded and
// re-encoded), per the FEATURE-002 finding that gateway metadata belongs in
// Meta, not spliced into Arguments.
func callTool(ctx context.Context, backend *egress.Backend, req mcp.Request) (mcp.Result, error) {
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok {
		return nil, fmt.Errorf("proxy: unexpected params type %T for %s", req.GetParams(), methodCallTool)
	}

	return backend.CallTool(ctx, &mcp.CallToolParams{
		Meta:      params.Meta,
		Name:      params.Name,
		Arguments: json.RawMessage(params.Arguments),
	})
}
