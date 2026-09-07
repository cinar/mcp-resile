package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cinar/mcp-resile/internal/egress"
	"github.com/cinar/mcp-resile/internal/ingress"
	"github.com/cinar/mcp-resile/internal/proxy"
)

type echoInput struct {
	Text string `json:"text" jsonschema:"text to echo back"`
}

type echoOutput struct {
	Text string `json:"text"`
}

// newTestBackend starts a real MCP server, standing in for a customer's
// backend, exposing a single "echo" tool.
func newTestBackend(t *testing.T) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "echo",
		Description: "Echoes the given text back",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
		return nil, echoOutput{Text: in.Text}, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer.URL
}

// newTestGateway dials backendURL and stands up the gateway's own ingress
// server with the proxy middleware attached, returning its URL.
func newTestGateway(t *testing.T, backendURL string) string {
	t.Helper()

	ctx := context.Background()
	backend, err := egress.Dial(ctx, "mcp-resile", "test", backendURL)
	if err != nil {
		t.Fatalf("egress.Dial: %v", err)
	}
	t.Cleanup(func() { backend.Close() })

	server := ingress.NewServer("mcp-resile", "test")
	server.AddReceivingMiddleware(proxy.Middleware(backend))

	httpServer := httptest.NewServer(ingress.NewHandler(server))
	t.Cleanup(httpServer.Close)

	return httpServer.URL
}

// TestPassthrough proves a client talking only to the gateway can list and
// call a tool it never registered itself, and gets the real backend's
// result back unmodified (FEATURE-006).
func TestPassthrough(t *testing.T) {
	ctx := context.Background()
	gatewayURL := newTestGateway(t, newTestBackend(t))

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gatewayURL}, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "echo" {
		t.Fatalf("ListTools = %+v, want a single %q tool", tools.Tools, "echo")
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": "hello"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool result.IsError = true, content: %+v", result.Content)
	}
	if got, want := result.StructuredContent.(map[string]any)["text"], "hello"; got != want {
		t.Errorf("StructuredContent.text = %v, want %v", got, want)
	}
}
