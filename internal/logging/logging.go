// Package logging builds the gateway's structured slog.Logger from
// telemetry.logging: config (spec.md §7), per the FEATURE-021 acceptance
// criterion: every request logs at info with tool name, backend, outcome,
// and latency as structured fields; errors log at warn/error with the
// mapped JSON-RPC code. This package only builds the *slog.Logger itself —
// what gets logged and at what level is proxy.Middleware's own call, since
// that's where the request outcome is known.
package logging

import (
	"log/slog"
	"os"

	"github.com/cinar/mcp-resile/internal/config"
)

// levels maps config.Logging.Level's validated values (config.Parse
// rejects anything else) to their slog.Level.
var levels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// New builds a *slog.Logger writing to os.Stdout, in the format and at the
// minimum level cfg specifies. cfg is assumed already validated by
// config.Parse (only the level/format values it accepts ever reach here);
// an unrecognized level defaults to info, and any format other than "text"
// produces JSON — the same fallback config.Parse's own validation exists to
// make unreachable in practice.
func New(cfg config.Logging) *slog.Logger {
	level, ok := levels[cfg.Level]
	if !ok {
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}
