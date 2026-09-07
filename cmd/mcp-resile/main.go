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

// run loads cfg, dials the one V1 backend, and serves the gateway's own MCP
// server over Streamable HTTP until ListenAndServe fails.
func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	backendCfg := cfg.Backends[0]
	backend, err := egress.Dial(context.Background(), serverName, version.Version, backendCfg.URL)
	if err != nil {
		return fmt.Errorf("dialing backend %s: %w", backendCfg.ID, err)
	}
	defer backend.Close()

	server := ingress.NewServer(serverName, version.Version)
	server.AddReceivingMiddleware(proxy.Middleware(backend))

	httpServer := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      ingress.NewHandler(server),
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeout),
		WriteTimeout: time.Duration(cfg.Server.WriteTimeout),
	}

	log.Printf("mcp-resile %s listening on %s, proxying to %s (%s)", version.Version, cfg.Server.Listen, backendCfg.ID, backendCfg.URL)
	return httpServer.ListenAndServe()
}
