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

	"github.com/cinar/mcp-resile/internal/config"
	"github.com/cinar/mcp-resile/internal/egress"
	"github.com/cinar/mcp-resile/internal/ingress"
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

	server := ingress.NewServer(serverName, version.Version, proxy.MergeCapabilities(routes))
	router.Attach(server)
	server.AddReceivingMiddleware(proxy.Middleware(routes, router, policies))

	httpServer := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      ingress.NewHandler(server),
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeout),
		WriteTimeout: time.Duration(cfg.Server.WriteTimeout),
	}

	log.Printf("mcp-resile %s listening on %s, proxying to %d backend(s)", version.Version, cfg.Server.Listen, len(cfg.Backends))
	return httpServer.ListenAndServe()
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
