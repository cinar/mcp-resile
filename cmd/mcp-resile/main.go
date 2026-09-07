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

	routes, closeBackends, err := dialBackends(cfg.Backends)
	if err != nil {
		return err
	}
	defer closeBackends()

	server := ingress.NewServer(serverName, version.Version)
	server.AddReceivingMiddleware(proxy.Middleware(routes))

	httpServer := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      ingress.NewHandler(server),
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeout),
		WriteTimeout: time.Duration(cfg.Server.WriteTimeout),
	}

	log.Printf("mcp-resile %s listening on %s, proxying to %d backend(s)", version.Version, cfg.Server.Listen, len(cfg.Backends))
	return httpServer.ListenAndServe()
}

// dialBackends opens an egress session to every configured backend. If any
// dial fails, the ones that already succeeded are closed before returning
// the error; the gateway either starts fully connected or not at all
// (spreading startup resilience across partial backend outages is
// FEATURE-008).
func dialBackends(backends []config.Backend) (routes []proxy.Route, closeAll func(), err error) {
	dialed := make([]*egress.Backend, 0, len(backends))
	closeAll = func() {
		for _, backend := range dialed {
			backend.Close()
		}
	}

	for _, backendCfg := range backends {
		backend, dialErr := egress.Dial(context.Background(), serverName, version.Version, backendCfg.URL)
		if dialErr != nil {
			closeAll()
			return nil, func() {}, fmt.Errorf("dialing backend %s: %w", backendCfg.ID, dialErr)
		}

		dialed = append(dialed, backend)
		routes = append(routes, proxy.Route{Prefix: backendCfg.Prefix, Backend: backend})
	}

	return routes, closeAll, nil
}
