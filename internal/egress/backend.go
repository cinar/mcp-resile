// Package egress opens and drives MCP client sessions toward backend
// servers over HTTP, per spec.md §3.3.
package egress

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// backendLogLevel is the level the gateway subscribes at on every backend
// that advertises logging support, so notifications/message actually flows
// (a backend sends none until a level is set, per spec.md/FEATURE-009); the
// gateway forwards whatever it receives on to clients rather than filtering,
// so it asks for the widest level.
const backendLogLevel = mcp.LoggingLevel("debug")

// DialOptions configures the notification handlers a Backend forwards
// backend-initiated notifications to. Both are optional; a nil handler
// means that notification kind is simply not forwarded.
type DialOptions struct {
	// OnProgress is called for every notifications/progress received from
	// the backend.
	OnProgress func(context.Context, *mcp.ProgressNotificationClientRequest)
	// OnLog is called for every notifications/message received from the
	// backend.
	OnLog func(context.Context, *mcp.LoggingMessageRequest)
}

// Backend is an MCP client session to a single backend server.
type Backend struct {
	session *mcp.ClientSession
}

// Dial opens an MCP client session to the backend MCP server at url, which
// must speak the Streamable HTTP transport.
func Dial(ctx context.Context, name, version, url string, opts DialOptions) (*Backend, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: version}, &mcp.ClientOptions{
		ProgressNotificationHandler: opts.OnProgress,
		LoggingMessageHandler:       opts.OnLog,
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		return nil, err
	}
	backend := &Backend{session: session}

	if session.InitializeResult().Capabilities.Logging != nil {
		if err := session.SetLoggingLevel(ctx, &mcp.SetLoggingLevelParams{Level: backendLogLevel}); err != nil {
			backend.Close()
			return nil, err
		}
	}

	return backend, nil
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
