package ingress_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cinar/mcp-resile/internal/ingress"
)

// TestInitialize proves the gateway accepts a Streamable HTTP MCP
// connection from a real client and completes initialize (FEATURE-003).
func TestInitialize(t *testing.T) {
	ctx := context.Background()

	server := ingress.NewServer("mcp-resile", "test", nil)
	httpServer := httptest.NewServer(ingress.NewHandler(server))
	defer httpServer.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer session.Close()

	result := session.InitializeResult()
	if result == nil {
		t.Fatal("InitializeResult() = nil, want a completed initialize handshake")
	}
	if got, want := result.ServerInfo.Name, "mcp-resile"; got != want {
		t.Errorf("ServerInfo.Name = %q, want %q", got, want)
	}

	if err := session.Ping(ctx, nil); err != nil {
		t.Errorf("Ping after initialize: %v", err)
	}
}
