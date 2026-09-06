// Package ingress builds the gateway's client-facing MCP server and serves
// it over Streamable HTTP, per spec.md §3.2.
package ingress

import (
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewServer creates the gateway's MCP server identity. It exposes no tools
// or middleware of its own yet; routing to backends is wired in by later
// features (FEATURE-006 onward).
func NewServer(name, version string) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, nil)
}

// NewHandler returns an http.Handler that serves server over the Streamable
// HTTP transport, the one ingress transport supported in V1.
func NewHandler(server *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
}
