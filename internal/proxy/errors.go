package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/cinar/resile"
	"github.com/cinar/resile/circuit"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/cinar/mcp-resile/internal/config"
)

// Custom JSON-RPC codes for resile policy rejections, per spec.md §6. They
// live at -33000 and below, clear of the -32768..-32000 band JSON-RPC (and,
// within it, -32020..-32099 for MCP itself) reserves.
const (
	codeRateLimitExceeded = -33001
	codeCircuitOpen       = -33002
	codeExecutionTimeout  = -33003
)

// mapResilienceError translates an error out of resile.Do's policy pipeline
// into the exact code/message/payload spec.md §6 defines for it, so a
// circuit-open, rate-limit, or timeout/min-deadline-threshold rejection
// reaches the client as a structured, LLM-actionable error rather than a
// generic internal one. rl is the rate limit configured on the policy that
// produced err, if any — used to compute retry_after_ms, since
// resile.RateLimiter itself exposes no such value.
//
// Any other error — the dispatch's own error once retries are exhausted, or
// context.Canceled from a disconnected client — is returned unchanged: it
// isn't a resile policy rejection, so §6's mapping doesn't apply to it.
func mapResilienceError(err error, rl *config.RateLimit) error {
	switch {
	case errors.Is(err, circuit.ErrCircuitOpen):
		return &jsonrpc.Error{
			Code:    codeCircuitOpen,
			Message: "Service unavailable",
			Data:    mustMarshal(circuitOpenData{Reason: "circuit_open"}),
		}

	case errors.Is(err, resile.ErrRateLimitExceeded):
		return &jsonrpc.Error{
			Code:    codeRateLimitExceeded,
			Message: "Rate limit exceeded",
			Data: mustMarshal(rateLimitData{
				RetryAfterMs: retryAfterMillis(rl),
				Reason:       "rate_limited",
			}),
		}

	// Both resile's own request timeout (WithTimeout) and its
	// min-deadline-threshold check (WithMinDeadlineThreshold, FEATURE-015)
	// surface as the bare context.DeadlineExceeded, and §6 has a single
	// "Execution Timeout" row covering both, so one case handles them.
	// context.Canceled is deliberately not matched here: that's a
	// disconnected client, not a resile policy rejection.
	case errors.Is(err, context.DeadlineExceeded):
		return &jsonrpc.Error{
			Code:    codeExecutionTimeout,
			Message: "Tool execution timeout",
			Data:    mustMarshal(timeoutData{Hint: "narrow the request scope or retry later"}),
		}

	default:
		return err
	}
}

type circuitOpenData struct {
	Reason string `json:"reason"`
}

type rateLimitData struct {
	RetryAfterMs int64  `json:"retry_after_ms"`
	Reason       string `json:"reason"`
}

type timeoutData struct {
	Hint string `json:"hint"`
}

// retryAfterMillis estimates the time until the rate limiter's next token,
// as the average interval between tokens (interval / rate). rl is nil, or
// its Rate is non-positive, only when called with a rate limit that isn't
// actually configured, which resile.ErrRateLimitExceeded should never
// surface for; 0 is returned defensively rather than dividing by zero.
func retryAfterMillis(rl *config.RateLimit) int64 {
	if rl == nil || rl.Rate <= 0 {
		return 0
	}
	return int64(math.Round(float64(time.Duration(rl.Interval).Milliseconds()) / rl.Rate))
}

// mustMarshal encodes v, one of the payload structs above, none of which can
// ever fail to marshal (no channels, funcs, or cycles).
func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
