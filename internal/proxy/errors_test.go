package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cinar/resile"
	"github.com/cinar/resile/circuit"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/cinar/mcp-resile/internal/config"
)

// TestMapResilienceError is the FEATURE-014 acceptance criterion's
// table-driven test: each resile policy rejection maps to the exact
// code/message/payload shape spec.md §6 documents, and anything else
// (the dispatch's own error, or a cancelled request) passes through
// unchanged.
func TestMapResilienceError(t *testing.T) {
	rl := &config.RateLimit{Rate: 2, Interval: config.Duration(time.Second)}
	errBackend := errors.New("backend exploded")

	tests := []struct {
		name        string
		err         error
		rl          *config.RateLimit
		wantCode    int64
		wantMessage string
		wantData    map[string]any
		wantSame    bool // err should pass through unchanged
	}{
		{
			name:        "circuit open",
			err:         fmt.Errorf("dispatch: %w", circuit.ErrCircuitOpen),
			wantCode:    codeCircuitOpen,
			wantMessage: "Service unavailable",
			wantData:    map[string]any{"reason": "circuit_open"},
		},
		{
			name:        "rate limit exceeded",
			err:         fmt.Errorf("dispatch: %w", resile.ErrRateLimitExceeded),
			rl:          rl,
			wantCode:    codeRateLimitExceeded,
			wantMessage: "Rate limit exceeded",
			wantData:    map[string]any{"reason": "rate_limited", "retry_after_ms": float64(500)},
		},
		{
			name:        "rate limit exceeded without a configured limit",
			err:         resile.ErrRateLimitExceeded,
			rl:          nil,
			wantCode:    codeRateLimitExceeded,
			wantMessage: "Rate limit exceeded",
			wantData:    map[string]any{"reason": "rate_limited", "retry_after_ms": float64(0)},
		},
		{
			name:        "deadline exceeded (timeout or min-deadline-threshold)",
			err:         fmt.Errorf("dispatch: %w", context.DeadlineExceeded),
			wantCode:    codeExecutionTimeout,
			wantMessage: "Tool execution timeout",
			wantData:    map[string]any{"hint": "narrow the request scope or retry later"},
		},
		{
			name:     "context canceled passes through unchanged",
			err:      context.Canceled,
			wantSame: true,
		},
		{
			name:     "backend's own error passes through unchanged",
			err:      errBackend,
			wantSame: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapResilienceError(tc.err, tc.rl)

			if tc.wantSame {
				if got != tc.err {
					t.Fatalf("mapResilienceError(%v) = %v, want it unchanged", tc.err, got)
				}
				return
			}

			var rpcErr *jsonrpc.Error
			if !errors.As(got, &rpcErr) {
				t.Fatalf("mapResilienceError(%v) = %v (%T), want a *jsonrpc.Error", tc.err, got, got)
			}

			if rpcErr.Code != tc.wantCode {
				t.Errorf("Code = %d, want %d", rpcErr.Code, tc.wantCode)
			}
			if rpcErr.Message != tc.wantMessage {
				t.Errorf("Message = %q, want %q", rpcErr.Message, tc.wantMessage)
			}

			var data map[string]any
			if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
				t.Fatalf("Data %s did not unmarshal as JSON: %v", rpcErr.Data, err)
			}
			if len(data) != len(tc.wantData) {
				t.Errorf("Data = %v, want %v", data, tc.wantData)
			}
			for k, want := range tc.wantData {
				if got := data[k]; got != want {
					t.Errorf("Data[%q] = %v, want %v", k, got, want)
				}
			}
		})
	}
}

func TestRetryAfterMillis(t *testing.T) {
	tests := []struct {
		name string
		rl   *config.RateLimit
		want int64
	}{
		{name: "nil rate limit", rl: nil, want: 0},
		{name: "zero rate", rl: &config.RateLimit{Rate: 0, Interval: config.Duration(time.Second)}, want: 0},
		{name: "100 per second", rl: &config.RateLimit{Rate: 100, Interval: config.Duration(time.Second)}, want: 10},
		{name: "1 per minute", rl: &config.RateLimit{Rate: 1, Interval: config.Duration(time.Minute)}, want: 60000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryAfterMillis(tc.rl); got != tc.want {
				t.Errorf("retryAfterMillis(%+v) = %d, want %d", tc.rl, got, tc.want)
			}
		})
	}
}
