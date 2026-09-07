package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cinar/mcp-resile/internal/config"
	"github.com/cinar/mcp-resile/internal/egress"
	"github.com/cinar/mcp-resile/internal/ingress"
	"github.com/cinar/mcp-resile/internal/metrics"
	"github.com/cinar/mcp-resile/internal/policy"
	"github.com/cinar/mcp-resile/internal/proxy"
)

// setupBenchmarkGateway wires a real backend behind a real gateway — the
// same egress.Dial/ingress.NewServer/proxy.Middleware/httptest.NewServer
// stack the rest of this package's tests use, not a synthetic stand-in —
// and returns a connected client session, so a benchmark measures the
// actual hot path (spec.md §12 FEATURE-023: "validate → policy match →
// resile pipeline → dispatch") end to end, including the real Streamable
// HTTP round trip to a real backend process.
func setupBenchmarkGateway(b *testing.B, policies ...config.Policy) *mcp.ClientSession {
	b.Helper()

	backendServer := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "bench"}, nil)
	mcp.AddTool(backendServer, &mcp.Tool{
		Name:        "echo",
		Description: "Echoes the given text back",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
		return nil, echoOutput{Text: in.Text}, nil
	})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return backendServer
	}, nil))
	b.Cleanup(backendHTTP.Close)

	backend, err := egress.Dial(context.Background(), "mcp-resile", "bench", backendHTTP.URL, egress.DialOptions{})
	if err != nil {
		b.Fatalf("egress.Dial: %v", err)
	}
	b.Cleanup(func() { backend.Close() })

	routes := []proxy.Route{{Prefix: "", Backend: backend}}
	router := proxy.NewNotificationRouter()
	server := ingress.NewServer("mcp-resile", "bench", proxy.MergeCapabilities(routes))
	router.Attach(server)
	server.AddReceivingMiddleware(proxy.Middleware(routes, router, policy.NewResolver(policies), 1<<20, metrics.New(), discardLogger))

	gatewayHTTP := httptest.NewServer(ingress.NewHandler(server))
	b.Cleanup(gatewayHTTP.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "bench-client", Version: "v0.0.1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: gatewayHTTP.URL}, nil)
	if err != nil {
		b.Fatalf("client.Connect: %v", err)
	}
	b.Cleanup(func() { session.Close() })

	return session
}

// BenchmarkCallToolNoPolicy measures the hot path for a tool name matching
// no policy: schema validation against the tool's cached inputSchema
// (FEATURE-017), policy resolution (a miss), and dispatch through
// baseResilienceOptions' panic-recovery-only resile.Do call (FEATURE-016) —
// the floor cost every tools/call pays, with no circuit breaker, retries,
// or rate limiter in the mix.
func BenchmarkCallToolNoPolicy(b *testing.B) {
	session := setupBenchmarkGateway(b)
	ctx := context.Background()
	args := map[string]any{"text": "hello, world"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: args})
		if err != nil {
			b.Fatalf("CallTool: %v", err)
		}
		if result.IsError {
			b.Fatalf("CallTool result.IsError = true, content: %+v", result.Content)
		}
	}
}

// BenchmarkCallToolWithFullPipeline measures the same hot path with a
// policy configured that exercises every resile primitive FEATURE-011/012/
// 013/015 wire up — circuit breaker, retries, rate limiter, min-deadline
// threshold — all on the success path (nothing actually opens, retries, or
// rejects during the run), isolating the pipeline's own per-call overhead
// from a real failure/rejection's.
func BenchmarkCallToolWithFullPipeline(b *testing.B) {
	session := setupBenchmarkGateway(b, config.Policy{
		ToolPattern: "echo",
		Resilience: config.Resilience{
			CircuitBreaker: &config.CircuitBreaker{
				FailureRate:    50,
				WindowDuration: config.Duration(time.Minute),
				ResetTimeout:   config.Duration(time.Minute),
			},
			Retries: &config.Retries{
				MaxAttempts: 3,
				BaseDelay:   config.Duration(10 * time.Millisecond),
				MaxDelay:    config.Duration(100 * time.Millisecond),
				Backoff:     "full_jitter",
			},
			RateLimit: &config.RateLimit{
				Rate:     1_000_000,
				Interval: config.Duration(time.Second),
			},
			MinDeadlineThreshold: config.Duration(time.Millisecond),
		},
	})
	ctx := context.Background()
	args := map[string]any{"text": "hello, world"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: args})
		if err != nil {
			b.Fatalf("CallTool: %v", err)
		}
		if result.IsError {
			b.Fatalf("CallTool result.IsError = true, content: %+v", result.Content)
		}
	}
}
