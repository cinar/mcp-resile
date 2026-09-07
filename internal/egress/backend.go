// Package egress opens and drives MCP client sessions toward backend
// servers over HTTP, per spec.md §3.3.
package egress

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Backend is an MCP client session to a single backend server.
type Backend struct {
	session *mcp.ClientSession
}

// Dial opens an MCP client session to the backend MCP server at url, which
// must speak the Streamable HTTP transport.
func Dial(ctx context.Context, name, version, url string) (*Backend, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: version}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		return nil, err
	}
	return &Backend{session: session}, nil
}

// ListTools forwards tools/list to the backend.
func (b *Backend) ListTools(ctx context.Context) (*mcp.ListToolsResult, error) {
	return b.session.ListTools(ctx, nil)
}

// CallTool forwards tools/call to the backend.
func (b *Backend) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return b.session.CallTool(ctx, params)
}

// Capabilities returns the capabilities the backend advertised in its
// initialize response.
func (b *Backend) Capabilities() *mcp.ServerCapabilities {
	return b.session.InitializeResult().Capabilities
}

// Close ends the backend session.
func (b *Backend) Close() error {
	return b.session.Close()
}
