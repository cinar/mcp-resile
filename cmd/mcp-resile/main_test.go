package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cinar/mcp-resile/internal/config"
)

// TestDialBackendsSkipsUnreachable proves a backend that's down at startup
// doesn't prevent another, reachable backend from becoming a usable route
// (FEATURE-008).
func TestDialBackendsSkipsUnreachable(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "test"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	backends := []config.Backend{
		{ID: "down", Prefix: "down_", URL: "http://127.0.0.1:1"},
		{ID: "up", Prefix: "up_", URL: httpServer.URL},
	}

	routes, closeAll := dialBackends(backends)
	defer closeAll()

	if len(routes) != 1 {
		t.Fatalf("dialBackends returned %d routes, want 1 (only the reachable backend)", len(routes))
	}
	if routes[0].Prefix != "up_" {
		t.Errorf("routes[0].Prefix = %q, want %q", routes[0].Prefix, "up_")
	}
}
