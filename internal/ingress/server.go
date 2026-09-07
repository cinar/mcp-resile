// Package ingress builds the gateway's client-facing MCP server and serves
// it over Streamable HTTP, per spec.md §3.2.
package ingress

import (
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewServer creates the gateway's MCP server identity, advertising caps as
// its capabilities. It exposes no tools, resources, or prompts of its own;
// routing to backends is wired in via middleware by later features
// (FEATURE-006 onward). Since the SDK only infers capabilities from a
// server's own registered features, caps must reflect whatever the proxied
// backends actually support (see proxy.MergeCapabilities); a nil caps falls
// back to the SDK's bare defaults.
func NewServer(name, version string, caps *mcp.ServerCapabilities) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, &mcp.ServerOptions{Capabilities: caps})
}

// NewHandler returns an http.Handler that serves server over the Streamable
// HTTP transport, the one ingress transport supported in V1.
func NewHandler(server *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
}
