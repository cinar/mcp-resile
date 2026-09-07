// Command mcp-resile is the resilience gateway/reverse proxy for the
// Model Context Protocol.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/cinar/mcp-resile/internal/auth"
	"github.com/cinar/mcp-resile/internal/config"
	"github.com/cinar/mcp-resile/internal/egress"
	"github.com/cinar/mcp-resile/internal/ingress"
	"github.com/cinar/mcp-resile/internal/metrics"
	"github.com/cinar/mcp-resile/internal/policy"
	"github.com/cinar/mcp-resile/internal/proxy"
	"github.com/cinar/mcp-resile/internal/version"
)

const serverName = "mcp-resile"

func main() {
	configPath := flag.String("config", "mcp-resile.yaml", "path to the mcp-resile.yaml config file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		log.Fatal(err)
	}
}

// run loads cfg, dials every configured backend, and serves the gateway's
// own MCP server over Streamable HTTP until ListenAndServe fails.
func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	router := proxy.NewNotificationRouter()

	routes, closeBackends := dialBackends(cfg.Backends, router)
	defer closeBackends()

	policies := policy.NewResolver(cfg.Policies)

	m := metrics.New()
	for _, p := range policies.Policies() {
		if cb := p.CircuitBreaker(); cb != nil {
			m.WatchCircuitBreaker(p.ToolPattern, cb)
		}
	}
	if cfg.Telemetry.Metrics.Enabled {
		serveMetrics(cfg.Telemetry.Metrics, m)
	}

	server := ingress.NewServer(serverName, version.Version, proxy.MergeCapabilities(routes))
	router.Attach(server)
	server.AddReceivingMiddleware(proxy.Middleware(routes, router, policies, cfg.Server.MaxResponseBytes, m))

	httpServer := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      auth.Middleware(cfg.Auth, ingress.NewHandler(server)),
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeout),
		WriteTimeout: time.Duration(cfg.Server.WriteTimeout),
	}

	log.Printf("mcp-resile %s listening on %s, proxying to %d backend(s)", version.Version, cfg.Server.Listen, len(cfg.Backends))
	return httpServer.ListenAndServe()
}

// serveMetrics starts the Prometheus /metrics endpoint on its own listener
// (telemetry.metrics.port), separate from the gateway's own server.listen,
// so scraping never competes with MCP traffic for the same port or routes
// (FEATURE-020). It runs in the background; a failure here logs but doesn't
// abort startup, matching dialBackends' own "one down piece shouldn't take
// the rest down" approach.
func serveMetrics(cfg config.Metrics, m *metrics.Metrics) {
	mux := http.NewServeMux()
	mux.Handle(cfg.Path, m.Handler())

	addr := fmt.Sprintf(":%d", cfg.Port)
	go func() {
		log.Printf("metrics listening on %s%s", addr, cfg.Path)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("metrics server stopped: %v", err)
		}
	}()
}

// dialBackends opens an egress session to every configured backend that is
// currently reachable, wiring router's handlers so backend notifications
// get forwarded to clients. A backend that fails to dial is logged and
// skipped rather than aborting startup, so one down backend never blocks
// the others from being usable (FEATURE-008); the gateway simply starts
// with whatever routes it managed to establish, even zero.
func dialBackends(backends []config.Backend, router *proxy.NotificationRouter) (routes []proxy.Route, closeAll func()) {
	dialed := make([]*egress.Backend, 0, len(backends))
	closeAll = func() {
		for _, backend := range dialed {
			backend.Close()
		}
	}

	for _, backendCfg := range backends {
		backend, err := egress.Dial(context.Background(), serverName, version.Version, backendCfg.URL, egress.DialOptions{
			OnProgress: router.HandleProgress,
			OnLog:      router.HandleLog,
		})
		if err != nil {
			log.Printf("backend %s (%s) unreachable at startup, skipping: %v", backendCfg.ID, backendCfg.URL, err)
			continue
		}

		dialed = append(dialed, backend)
		routes = append(routes, proxy.Route{Prefix: backendCfg.Prefix, Backend: backend})
	}

	return routes, closeAll
}
