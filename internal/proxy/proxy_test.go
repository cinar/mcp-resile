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
// backend, exposing a single tool named toolName that echoes its input.
func newTestBackend(t *testing.T, toolName string) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        toolName,
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

// newTestResourceBackend starts a real MCP server exposing a single
// resource and no tools, standing in for a backend whose only capability is
// resources.
func newTestResourceBackend(t *testing.T) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "test"}, nil)
	server.AddResource(&mcp.Resource{
		URI:  "test://thing",
		Name: "thing",
	}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer.URL
}

// dialTestBackend dials url and registers its Close for cleanup.
func dialTestBackend(t *testing.T, url string) *egress.Backend {
	t.Helper()

	backend, err := egress.Dial(context.Background(), "mcp-resile", "test", url)
	if err != nil {
		t.Fatalf("egress.Dial: %v", err)
	}
	t.Cleanup(func() { backend.Close() })

	return backend
}

// newTestGateway stands up the gateway's own ingress server with the proxy
// middleware attached to routes, returning its URL.
func newTestGateway(t *testing.T, routes []proxy.Route) string {
	t.Helper()

	server := ingress.NewServer("mcp-resile", "test", proxy.MergeCapabilities(routes))
	server.AddReceivingMiddleware(proxy.Middleware(routes))

	httpServer := httptest.NewServer(ingress.NewHandler(server))
	t.Cleanup(httpServer.Close)

	return httpServer.URL
}

func connect(t *testing.T, url string) *mcp.ClientSession {
	t.Helper()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	return session
}

// TestPassthrough proves a client talking only to the gateway can list and
// call a tool it never registered itself, and gets the real backend's
// result back unmodified, with an empty (catch-all) prefix (FEATURE-006).
func TestPassthrough(t *testing.T) {
	ctx := context.Background()
	backend := dialTestBackend(t, newTestBackend(t, "echo"))
	gatewayURL := newTestGateway(t, []proxy.Route{{Prefix: "", Backend: backend}})

	session := connect(t, gatewayURL)

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

// TestMultiBackendNamespaceRouting proves that with two backends behind
// distinct prefixes, tools/list surfaces both, correctly prefixed, and
// tools/call routes each prefixed name to the right backend (FEATURE-007).
func TestMultiBackendNamespaceRouting(t *testing.T) {
	ctx := context.Background()

	dbBackend := dialTestBackend(t, newTestBackend(t, "query"))
	jiraBackend := dialTestBackend(t, newTestBackend(t, "query"))

	gatewayURL := newTestGateway(t, []proxy.Route{
		{Prefix: "db_", Backend: dbBackend},
		{Prefix: "jira_", Backend: jiraBackend},
	})
	session := connect(t, gatewayURL)

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make(map[string]bool)
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	if !names["db_query"] || !names["jira_query"] {
		t.Fatalf("ListTools = %+v, want db_query and jira_query", tools.Tools)
	}

	dbResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "db_query",
		Arguments: map[string]any{"text": "from-db"},
	})
	if err != nil {
		t.Fatalf("CallTool db_query: %v", err)
	}
	if got := dbResult.StructuredContent.(map[string]any)["text"]; got != "from-db" {
		t.Errorf("db_query result = %v, want from-db", got)
	}

	jiraResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "jira_query",
		Arguments: map[string]any{"text": "from-jira"},
	})
	if err != nil {
		t.Fatalf("CallTool jira_query: %v", err)
	}
	if got := jiraResult.StructuredContent.(map[string]any)["text"]; got != "from-jira" {
		t.Errorf("jira_query result = %v, want from-jira", got)
	}
}

// TestCapabilityMerging proves the gateway's own initialize response
// reflects the union of its backends' capabilities, even though the gateway
// registers no tools/resources/prompts of its own (FEATURE-008). One
// backend offers only a tool, the other only a resource; the gateway must
// advertise both.
func TestCapabilityMerging(t *testing.T) {
	toolBackend := dialTestBackend(t, newTestBackend(t, "query"))
	resourceBackend := dialTestBackend(t, newTestResourceBackend(t))

	gatewayURL := newTestGateway(t, []proxy.Route{
		{Prefix: "db_", Backend: toolBackend},
		{Prefix: "res_", Backend: resourceBackend},
	})
	session := connect(t, gatewayURL)

	caps := session.InitializeResult().Capabilities
	if caps == nil {
		t.Fatal("InitializeResult().Capabilities = nil, want a merged set of capabilities")
	}
	if caps.Tools == nil {
		t.Error("Capabilities.Tools = nil, want non-nil since a backend has a tool")
	}
	if caps.Resources == nil {
		t.Error("Capabilities.Resources = nil, want non-nil since a backend has a resource")
	}
	if caps.Prompts != nil {
		t.Errorf("Capabilities.Prompts = %+v, want nil since no backend has a prompt", caps.Prompts)
	}
}

// TestUnknownToolFallsThrough proves a tools/call for a name matching no
// route's prefix is reported as an unknown tool, not silently dropped.
func TestUnknownToolFallsThrough(t *testing.T) {
	ctx := context.Background()
	backend := dialTestBackend(t, newTestBackend(t, "query"))
	gatewayURL := newTestGateway(t, []proxy.Route{{Prefix: "db_", Backend: backend}})
	session := connect(t, gatewayURL)

	_, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "jira_query",
		Arguments: map[string]any{"text": "hello"},
	})
	if err == nil {
		t.Fatal("CallTool: got nil error for an unrouted tool name, want an unknown-tool error")
	}
}
