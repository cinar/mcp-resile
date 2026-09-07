package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	neturl "net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cinar/mcp-resile/internal/config"
	"github.com/cinar/mcp-resile/internal/egress"
	"github.com/cinar/mcp-resile/internal/ingress"
	"github.com/cinar/mcp-resile/internal/metrics"
	"github.com/cinar/mcp-resile/internal/policy"
	"github.com/cinar/mcp-resile/internal/proxy"
)

// scrapeMetrics renders m's Prometheus exposition format as a string, so
// tests can assert on it directly rather than reaching into unexported
// collector state.
func scrapeMetrics(t *testing.T, m *metrics.Metrics) string {
	t.Helper()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("reading /metrics response: %v", err)
	}
	return string(body)
}

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

// newCountingBackend starts a real MCP server exposing a single tool named
// toolName that echoes its input, and returns its URL along with a counter
// of the tools/call requests it has received — for proving how many dispatch
// attempts a call actually made it to the backend (FEATURE-012).
func newCountingBackend(t *testing.T, toolName string) (url string, calls *atomic.Int32) {
	t.Helper()

	calls = &atomic.Int32{}
	server := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        toolName,
		Description: "Echoes the given text back",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
		calls.Add(1)
		return nil, echoOutput{Text: in.Text}, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer.URL, calls
}

// newFlakyBackend starts a real MCP server exposing a single tool named
// toolName that echoes its input, fronted by a reverse proxy (so the real
// transport, headers, and any SSE streaming are handled correctly by the
// standard library rather than a hand-rolled stand-in) whose first
// failCount tools/call requests get a 503 response before every later one
// reaches the server unmodified. A 503 is one of the HTTP statuses go-sdk's
// streamable HTTP client itself treats as transient (wrapping it with its
// own jsonrpc2.ErrRejected, code -32005) — exactly the signal
// proxy.isTransientError keys off of — so this exercises the real go-sdk
// code path FEATURE-012 depends on, not a synthetic stand-in for it. The
// returned counter tracks every tools/call request the wrapper has seen,
// regardless of the tool name it targets or whether it was rejected —
// i.e. the number of dispatch attempts made over the wire.
func newFlakyBackend(t *testing.T, toolName string, failCount int) (backendURL string, attempts *atomic.Int32) {
	t.Helper()

	realURL, _ := newCountingBackend(t, toolName)
	target, err := neturl.Parse(realURL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	reverseProxy := httputil.NewSingleHostReverseProxy(target)

	attempts = &atomic.Int32{}
	flaky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte(`"tools/call"`)) {
				if n := attempts.Add(1); n <= int32(failCount) {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
		}
		reverseProxy.ServeHTTP(w, r)
	})

	httpServer := httptest.NewServer(flaky)
	t.Cleanup(httpServer.Close)

	return httpServer.URL, attempts
}

// newListCountingBackend starts a real MCP server exposing a single tool
// named toolName that echoes its input (via newCountingBackend), fronted by
// a reverse proxy that additionally counts tools/list requests it forwards
// — used to prove a tool's inputSchema is resolved from the backend once
// and cached, not refetched on every tools/call (FEATURE-017).
func newListCountingBackend(t *testing.T, toolName string) (backendURL string, listCalls *atomic.Int32) {
	t.Helper()

	realURL, _ := newCountingBackend(t, toolName)
	target, err := neturl.Parse(realURL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	reverseProxy := httputil.NewSingleHostReverseProxy(target)

	listCalls = &atomic.Int32{}
	counting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte(`"tools/list"`)) {
				listCalls.Add(1)
			}
		}
		reverseProxy.ServeHTTP(w, r)
	})

	httpServer := httptest.NewServer(counting)
	t.Cleanup(httpServer.Close)

	return httpServer.URL, listCalls
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

	backend, err := egress.Dial(context.Background(), "mcp-resile", "test", url, egress.DialOptions{})
	if err != nil {
		t.Fatalf("egress.Dial: %v", err)
	}
	t.Cleanup(func() { backend.Close() })

	return backend
}

// defaultTestMaxResponseBytes is a generous response-size budget for tests
// that aren't exercising FEATURE-018 truncation, so ordinary small test
// responses are never accidentally clamped.
const defaultTestMaxResponseBytes = 1 << 20

// newTestGateway stands up the gateway's own ingress server with the proxy
// middleware attached to routes, returning its URL. Its NotificationRouter
// is private to this gateway; tests exercising notification forwarding
// build their own wiring instead, since that requires sharing one router
// between the egress.Dial call and the gateway (see
// newRoutedTestSystem). policies is optional; most tests have none.
func newTestGateway(t *testing.T, routes []proxy.Route, policies ...config.Policy) string {
	t.Helper()
	return newTestGatewayWithMaxResponseBytes(t, routes, defaultTestMaxResponseBytes, policies...)
}

// newTestGatewayWithMaxResponseBytes is newTestGateway with an explicit
// response-size budget, for tests exercising FEATURE-018 truncation.
func newTestGatewayWithMaxResponseBytes(t *testing.T, routes []proxy.Route, maxResponseBytes int64, policies ...config.Policy) string {
	t.Helper()

	router := proxy.NewNotificationRouter()
	server := ingress.NewServer("mcp-resile", "test", proxy.MergeCapabilities(routes))
	router.Attach(server)
	server.AddReceivingMiddleware(proxy.Middleware(routes, router, policy.NewResolver(policies), maxResponseBytes, metrics.New()))

	httpServer := httptest.NewServer(ingress.NewHandler(server))
	t.Cleanup(httpServer.Close)

	return httpServer.URL
}

// newTestGatewayWithMetrics is newTestGateway, but also returns the
// *metrics.Metrics instance wired into the gateway's Middleware, for tests
// that assert on FEATURE-020's recorded metrics rather than just the
// JSON-RPC response.
func newTestGatewayWithMetrics(t *testing.T, routes []proxy.Route, policies ...config.Policy) (string, *metrics.Metrics) {
	t.Helper()

	m := metrics.New()
	router := proxy.NewNotificationRouter()
	server := ingress.NewServer("mcp-resile", "test", proxy.MergeCapabilities(routes))
	router.Attach(server)
	server.AddReceivingMiddleware(proxy.Middleware(routes, router, policy.NewResolver(policies), defaultTestMaxResponseBytes, m))

	httpServer := httptest.NewServer(ingress.NewHandler(server))
	t.Cleanup(httpServer.Close)

	return httpServer.URL, m
}

type notifyInput struct {
	Marker string `json:"marker"`
}

// newTestNotifyBackend starts a backend exposing a "notify" tool that, if
// the caller set a progress token, reports progress carrying that token and
// in.Marker before returning in.Marker as its result.
func newTestNotifyBackend(t *testing.T) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "notify",
		Description: "Reports progress, then echoes marker back",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in notifyInput) (*mcp.CallToolResult, echoOutput, error) {
		if token := req.Params.GetProgressToken(); token != nil {
			if err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: token,
				Message:       in.Marker,
				Progress:      1,
			}); err != nil {
				return nil, echoOutput{}, err
			}
		}
		return nil, echoOutput{Text: in.Marker}, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer.URL
}

// newRoutedTestSystem wires a single backend at backendURL into a gateway,
// sharing one NotificationRouter between them so backend-initiated
// notifications (progress, logging) are actually forwarded, and returns the
// gateway's URL. dialTestBackend/newTestGateway can't be composed for this,
// since egress.Dial (backend side) and the gateway's own server both need
// the same router instance.
func newRoutedTestSystem(t *testing.T, backendURL string) string {
	t.Helper()

	router := proxy.NewNotificationRouter()
	backend, err := egress.Dial(context.Background(), "mcp-resile", "test", backendURL, egress.DialOptions{
		OnProgress: router.HandleProgress,
		OnLog:      router.HandleLog,
	})
	if err != nil {
		t.Fatalf("egress.Dial: %v", err)
	}
	t.Cleanup(func() { backend.Close() })

	routes := []proxy.Route{{Prefix: "", Backend: backend}}
	server := ingress.NewServer("mcp-resile", "test", proxy.MergeCapabilities(routes))
	router.Attach(server)
	server.AddReceivingMiddleware(proxy.Middleware(routes, router, policy.NewResolver(nil), defaultTestMaxResponseBytes, metrics.New()))

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

// TestProgressNotificationForwarding proves a backend's
// notifications/progress reaches the client that made the call, with the
// client's own progress token restored (FEATURE-009).
func TestProgressNotificationForwarding(t *testing.T) {
	ctx := context.Background()
	gatewayURL := newRoutedTestSystem(t, newTestNotifyBackend(t))

	progress := make(chan *mcp.ProgressNotificationParams, 1)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			progress <- req.Params
		},
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gatewayURL}, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	params := &mcp.CallToolParams{Name: "notify", Arguments: map[string]any{"marker": "hello"}}
	params.SetProgressToken("client-token")

	result, err := session.CallTool(ctx, params)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool result.IsError = true: %+v", result.Content)
	}

	select {
	case got := <-progress:
		if got.Message != "hello" {
			t.Errorf("progress.Message = %q, want %q", got.Message, "hello")
		}
		if got.ProgressToken != "client-token" {
			t.Errorf("progress.ProgressToken = %v, want the client's own token %q restored", got.ProgressToken, "client-token")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notifications/progress to be forwarded")
	}
}

// TestProgressNotificationRoutedByClient proves two clients sharing one
// backend connection, who happen to pick the identical progress token,
// each get only their own progress notifications rather than each other's
// (FEATURE-009) — the scenario NotificationRouter's token rewriting exists
// for.
func TestProgressNotificationRoutedByClient(t *testing.T) {
	ctx := context.Background()
	gatewayURL := newRoutedTestSystem(t, newTestNotifyBackend(t))

	type testClient struct {
		session  *mcp.ClientSession
		progress chan *mcp.ProgressNotificationParams
	}
	newClient := func() testClient {
		progress := make(chan *mcp.ProgressNotificationParams, 1)
		c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, &mcp.ClientOptions{
			ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
				progress <- req.Params
			},
		})
		session, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gatewayURL}, nil)
		if err != nil {
			t.Fatalf("client.Connect: %v", err)
		}
		t.Cleanup(func() { session.Close() })
		return testClient{session: session, progress: progress}
	}

	a, b := newClient(), newClient()

	call := func(c testClient, marker string) error {
		params := &mcp.CallToolParams{Name: "notify", Arguments: map[string]any{"marker": marker}}
		params.SetProgressToken("same-token")
		_, err := c.session.CallTool(ctx, params)
		return err
	}

	errs := make(chan error, 2)
	go func() { errs <- call(a, "from-a") }()
	go func() { errs <- call(b, "from-b") }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	}

	for _, want := range []struct {
		client testClient
		marker string
	}{{a, "from-a"}, {b, "from-b"}} {
		select {
		case got := <-want.client.progress:
			if got.Message != want.marker {
				t.Errorf("progress.Message = %q, want %q", got.Message, want.marker)
			}
			if got.ProgressToken != "same-token" {
				t.Errorf("progress.ProgressToken = %v, want %q", got.ProgressToken, "same-token")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for progress for marker %q", want.marker)
		}
	}
}

// TestLogMessageBroadcast proves a backend's notifications/message reaches
// a connected client once it has opted in via logging/setLevel (FEATURE-009).
func TestLogMessageBroadcast(t *testing.T) {
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "log",
		Description: "Sends a log message back to the caller",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
		err := req.Session.Log(ctx, &mcp.LoggingMessageParams{Level: "info", Data: "hello from backend"})
		return nil, struct{}{}, err
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
	backendHTTP := httptest.NewServer(handler)
	t.Cleanup(backendHTTP.Close)

	gatewayURL := newRoutedTestSystem(t, backendHTTP.URL)

	logs := make(chan *mcp.LoggingMessageParams, 1)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, &mcp.ClientOptions{
		LoggingMessageHandler: func(_ context.Context, req *mcp.LoggingMessageRequest) {
			logs <- req.Params
		},
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gatewayURL}, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	if err := session.SetLoggingLevel(ctx, &mcp.SetLoggingLevelParams{Level: "debug"}); err != nil {
		t.Fatalf("SetLoggingLevel: %v", err)
	}

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "log", Arguments: map[string]any{}}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	select {
	case got := <-logs:
		if got.Data != "hello from backend" {
			t.Errorf("log.Data = %v, want %q", got.Data, "hello from backend")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notifications/message to be forwarded")
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

// TestCircuitBreakerOpensAfterSustainedFailures proves a policy's circuit
// breaker actually gates dispatch end-to-end (FEATURE-011): calling a tool
// name that doesn't exist on the backend fails every time via a real
// JSON-RPC error, and once the breaker trips, that keeps failing but with
// the breaker's own error, mapped to the -33002 "circuit open" code
// (FEATURE-014), not the backend's own error — meaning later calls in the
// run stopped reaching the backend at all.
func TestCircuitBreakerOpensAfterSustainedFailures(t *testing.T) {
	ctx := context.Background()
	backend := dialTestBackend(t, newTestBackend(t, "echo"))
	gatewayURL := newTestGateway(t, []proxy.Route{{Prefix: "", Backend: backend}}, config.Policy{
		ToolPattern: "boom",
		Resilience: config.Resilience{
			CircuitBreaker: &config.CircuitBreaker{
				FailureRate:    50,
				WindowDuration: config.Duration(time.Minute),
				ResetTimeout:   config.Duration(time.Minute),
			},
		},
	})
	session := connect(t, gatewayURL)

	var lastErr error
	for i := 0; i < 20; i++ {
		_, lastErr = session.CallTool(ctx, &mcp.CallToolParams{Name: "boom", Arguments: map[string]any{}})
		if lastErr == nil {
			t.Fatal("CallTool: got nil error for a tool the backend doesn't expose")
		}
	}

	var rpcErr *jsonrpc.Error
	if !errors.As(lastErr, &rpcErr) {
		t.Fatalf("after 20 straight failures, CallTool error = %v (%T), want a *jsonrpc.Error", lastErr, lastErr)
	}
	if rpcErr.Code != -33002 {
		t.Errorf("after 20 straight failures, CallTool error code = %d, want -33002 (circuit open)", rpcErr.Code)
	}
	if rpcErr.Message != "Service unavailable" {
		t.Errorf("after 20 straight failures, CallTool error message = %q, want %q", rpcErr.Message, "Service unavailable")
	}
}

// TestRetriesRecoverFromTransientFailure proves a policy's retries actually
// mask a transient backend failure end-to-end (FEATURE-012): the backend
// rejects the first two attempts with a 503 (transient), and the call still
// succeeds because the third attempt gets through.
func TestRetriesRecoverFromTransientFailure(t *testing.T) {
	ctx := context.Background()
	backendURL, attempts := newFlakyBackend(t, "echo", 2)
	backend := dialTestBackend(t, backendURL)
	gatewayURL := newTestGateway(t, []proxy.Route{{Prefix: "", Backend: backend}}, config.Policy{
		ToolPattern: "echo",
		Resilience: config.Resilience{
			Retries: &config.Retries{
				MaxAttempts: 3,
				BaseDelay:   config.Duration(time.Millisecond),
				MaxDelay:    config.Duration(5 * time.Millisecond),
			},
		},
	})
	session := connect(t, gatewayURL)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": "hello"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v, want it to succeed once retries exhaust the transient failures", err)
	}
	if result.IsError {
		t.Fatalf("CallTool result.IsError = true, content: %+v", result.Content)
	}
	if got, want := result.StructuredContent.(map[string]any)["text"], "hello"; got != want {
		t.Errorf("StructuredContent.text = %v, want %v", got, want)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("backend saw %d tools/call attempts, want exactly 3 (2 rejected, then the successful retry)", got)
	}
}

// TestRetriesDoNotRetryNonTransientErrors proves a non-transient error
// (here, an application-level "unknown tool" JSON-RPC error) is never
// retried (FEATURE-012), even though the policy allows up to 3 attempts:
// only one attempt should ever reach the backend.
func TestRetriesDoNotRetryNonTransientErrors(t *testing.T) {
	ctx := context.Background()
	backendURL, attempts := newFlakyBackend(t, "echo", 0)
	backend := dialTestBackend(t, backendURL)
	gatewayURL := newTestGateway(t, []proxy.Route{{Prefix: "", Backend: backend}}, config.Policy{
		ToolPattern: "boom",
		Resilience: config.Resilience{
			Retries: &config.Retries{
				MaxAttempts: 3,
				BaseDelay:   config.Duration(time.Millisecond),
				MaxDelay:    config.Duration(5 * time.Millisecond),
			},
		},
	})
	session := connect(t, gatewayURL)

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "boom", Arguments: map[string]any{}}); err == nil {
		t.Fatal("CallTool: got nil error for a tool the backend doesn't expose")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("backend saw %d tools/call attempts for a non-transient error, want exactly 1 (no retry)", got)
	}
}

// TestRateLimitRejectsExcessCalls proves a policy's rate limit actually
// gates dispatch end-to-end (FEATURE-013): with a bucket of 1 token per
// minute, a second call within that window is rejected without reaching
// the backend at all.
func TestRateLimitRejectsExcessCalls(t *testing.T) {
	ctx := context.Background()
	backendURL, calls := newCountingBackend(t, "echo")
	backend := dialTestBackend(t, backendURL)
	gatewayURL := newTestGateway(t, []proxy.Route{{Prefix: "", Backend: backend}}, config.Policy{
		ToolPattern: "echo",
		Resilience: config.Resilience{
			RateLimit: &config.RateLimit{
				Rate:     1,
				Interval: config.Duration(time.Minute),
			},
		},
	})
	session := connect(t, gatewayURL)

	first, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatalf("first CallTool: %v, want it to succeed (the bucket starts full)", err)
	}
	if first.IsError {
		t.Fatalf("first CallTool result.IsError = true, content: %+v", first.Content)
	}

	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi"}})
	if err == nil {
		t.Fatal("second CallTool: got nil error, want the rate limit to reject it")
	}

	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("second CallTool error = %v (%T), want a *jsonrpc.Error", err, err)
	}
	if rpcErr.Code != -33001 {
		t.Errorf("second CallTool error code = %d, want -33001 (rate limit exceeded)", rpcErr.Code)
	}
	if rpcErr.Message != "Rate limit exceeded" {
		t.Errorf("second CallTool error message = %q, want %q", rpcErr.Message, "Rate limit exceeded")
	}

	var data map[string]any
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		t.Fatalf("Data %s did not unmarshal as JSON: %v", rpcErr.Data, err)
	}
	if data["reason"] != "rate_limited" {
		t.Errorf(`Data["reason"] = %v, want "rate_limited"`, data["reason"])
	}
	if _, ok := data["retry_after_ms"]; !ok {
		t.Error(`Data missing "retry_after_ms"`)
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("backend tool handler ran %d times, want exactly 1 (the rate-limited call never reached it)", got)
	}
}

// TestSchemaValidationRejectsInvalidArguments proves tools/call arguments
// are validated against the tool's own inputSchema before dispatch
// (FEATURE-017): "echo"'s schema (inferred by mcp.AddTool from echoInput)
// requires text to be a string, so passing a number for it is rejected with
// -32602 "Invalid params", and the backend's own handler never runs.
func TestSchemaValidationRejectsInvalidArguments(t *testing.T) {
	ctx := context.Background()
	backendURL, calls := newCountingBackend(t, "echo")
	backend := dialTestBackend(t, backendURL)
	gatewayURL := newTestGateway(t, []proxy.Route{{Prefix: "", Backend: backend}})
	session := connect(t, gatewayURL)

	_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": 12345}})
	if err == nil {
		t.Fatal("CallTool: got nil error for arguments violating the tool's inputSchema")
	}

	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("CallTool error = %v (%T), want a *jsonrpc.Error", err, err)
	}
	if rpcErr.Code != -32602 {
		t.Errorf("CallTool error code = %d, want -32602 (Invalid params)", rpcErr.Code)
	}
	if rpcErr.Message != "Invalid params" {
		t.Errorf("CallTool error message = %q, want %q", rpcErr.Message, "Invalid params")
	}

	var data map[string]any
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		t.Fatalf("Data %s did not unmarshal as JSON: %v", rpcErr.Data, err)
	}
	if data["tool"] != "echo" {
		t.Errorf(`Data["tool"] = %v, want "echo"`, data["tool"])
	}
	if detail, _ := data["detail"].(string); detail == "" {
		t.Error(`Data["detail"] is empty, want a field-level validation message`)
	}

	if got := calls.Load(); got != 0 {
		t.Errorf("backend tool handler ran %d times, want 0 (the schema violation never reached it)", got)
	}
}

// TestSchemaValidationRejectsMissingRequiredField proves a missing required
// property is caught the same way as a type mismatch (FEATURE-017).
func TestSchemaValidationRejectsMissingRequiredField(t *testing.T) {
	ctx := context.Background()
	backendURL, calls := newCountingBackend(t, "echo")
	backend := dialTestBackend(t, backendURL)
	gatewayURL := newTestGateway(t, []proxy.Route{{Prefix: "", Backend: backend}})
	session := connect(t, gatewayURL)

	_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{}})
	if err == nil {
		t.Fatal("CallTool: got nil error for arguments missing a required field")
	}

	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("CallTool error = %v (%T), want a *jsonrpc.Error", err, err)
	}
	if rpcErr.Code != -32602 {
		t.Errorf("CallTool error code = %d, want -32602 (Invalid params)", rpcErr.Code)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("backend tool handler ran %d times, want 0 (the schema violation never reached it)", got)
	}
}

// TestSchemaCompiledOnce proves a tool's inputSchema is resolved from the
// backend once and cached, not recompiled/refetched on every tools/call
// (FEATURE-017's acceptance criterion): five valid calls to the same tool,
// with no client-side tools/list ever run first (so the very first call
// itself must trigger the cache-miss fetch), still only ever cause a single
// tools/list request to reach the backend.
func TestSchemaCompiledOnce(t *testing.T) {
	ctx := context.Background()
	backendURL, listCalls := newListCountingBackend(t, "echo")
	backend := dialTestBackend(t, backendURL)
	gatewayURL := newTestGateway(t, []proxy.Route{{Prefix: "", Backend: backend}})
	session := connect(t, gatewayURL)

	for i := 0; i < 5; i++ {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi"}})
		if err != nil {
			t.Fatalf("CallTool #%d: %v", i, err)
		}
		if result.IsError {
			t.Fatalf("CallTool #%d result.IsError = true, content: %+v", i, result.Content)
		}
	}

	if got := listCalls.Load(); got != 1 {
		t.Errorf("backend saw %d tools/list requests across 5 tools/call, want exactly 1 (the schema is cached after the first)", got)
	}
}

// TestResponseClampingTruncatesOversizedBackendResult proves
// max_response_bytes is enforced end-to-end (FEATURE-018): a real
// backend's response that exceeds the configured budget comes back
// truncated with the "[TRUNCATED: ...]" notice appended, and isError
// stays false.
func TestResponseClampingTruncatesOversizedBackendResult(t *testing.T) {
	ctx := context.Background()
	backendURL, _ := newCountingBackend(t, "echo")
	backend := dialTestBackend(t, backendURL)
	gatewayURL := newTestGatewayWithMaxResponseBytes(t, []proxy.Route{{Prefix: "", Backend: backend}}, 50)
	session := connect(t, gatewayURL)

	big := strings.Repeat("x", 10000)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": big}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, want false (a truncated response is still a successful call), content: %+v", result.Content)
	}
	if len(result.Content) == 0 {
		t.Fatal("result.Content is empty, want at least the truncation notice")
	}

	last, ok := result.Content[len(result.Content)-1].(*mcp.TextContent)
	if !ok || last.Text != "[TRUNCATED: response exceeded max_response_bytes]" {
		t.Fatalf("last content block = %+v, want the truncation notice", result.Content[len(result.Content)-1])
	}

	var totalText int
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			totalText += len(tc.Text)
		}
	}
	if totalText >= len(big) {
		t.Errorf("total text content = %d bytes, want it well under the original %d-byte response", totalText, len(big))
	}
}

// TestMetricsRecordedForRealDispatch proves FEATURE-020's request
// count/latency metrics are actually wired into a real tools/call dispatch,
// for both a successful call and a schema-validation rejection (which never
// reaches resile.Do), not just the metrics package's own unit tests.
func TestMetricsRecordedForRealDispatch(t *testing.T) {
	ctx := context.Background()
	backendURL := newTestBackend(t, "echo")
	backend := dialTestBackend(t, backendURL)
	gatewayURL, m := newTestGatewayWithMetrics(t, []proxy.Route{{Prefix: "", Backend: backend}})
	session := connect(t, gatewayURL)

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi"}}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	// "text" is required by echo's inputSchema (see newTestBackend); omitting
	// it triggers FEATURE-017's -32602 rejection before resile.Do ever runs.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{}}); err == nil {
		t.Fatal("CallTool with missing required argument succeeded, want a schema-validation error")
	}

	body := scrapeMetrics(t, m)
	if !strings.Contains(body, `mcp_resile_requests_total{outcome="success",tool="echo"} 1`) {
		t.Errorf("missing/wrong success count in:\n%s", body)
	}
	if !strings.Contains(body, `mcp_resile_requests_total{outcome="invalid_params",tool="echo"} 1`) {
		t.Errorf("missing/wrong invalid_params count in:\n%s", body)
	}
	if !strings.Contains(body, `mcp_resile_request_duration_seconds_count{tool="echo"} 2`) {
		t.Errorf("missing/wrong latency observation count in:\n%s", body)
	}
}
