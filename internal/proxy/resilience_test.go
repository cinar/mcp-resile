package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cinar/resile"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/cinar/mcp-resile/internal/config"
	"github.com/cinar/mcp-resile/internal/metrics"
	"github.com/cinar/mcp-resile/internal/policy"
)

// TestMinDeadlineThresholdAbortsBeforeDispatch proves resilienceOptions'
// min-deadline-threshold wiring (FEATURE-015) does what callTool relies on
// it for: given a ctx whose remaining deadline is already below the
// configured threshold, resile.Do aborts before ever invoking dispatch —
// the acceptance criterion's "not after a wasted round trip" — and the
// resulting context.DeadlineExceeded maps to the -33003 "Tool execution
// timeout" code (FEATURE-014).
func TestMinDeadlineThresholdAbortsBeforeDispatch(t *testing.T) {
	resolver := policy.NewResolver([]config.Policy{
		{
			ToolPattern: "db_read_*",
			Resilience: config.Resilience{
				MinDeadlineThreshold: config.Duration(time.Second),
			},
		},
	})
	p, ok := resolver.Resolve("db_read_users")
	if !ok {
		t.Fatal("Resolve should have matched db_read_*")
	}

	// The threshold (1s) is far larger than the ctx's remaining time (10ms),
	// so the abort fires on the very first check, no sleep required.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	dispatched := false
	dispatch := func(context.Context) (*struct{}, error) {
		dispatched = true
		return &struct{}{}, nil
	}

	_, err := resile.Do(ctx, dispatch, resilienceOptions(p, "db_read_users", metrics.New())...)
	if dispatched {
		t.Error("dispatch ran despite the remaining deadline being below min_deadline_threshold")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resile.Do error = %v, want context.DeadlineExceeded", err)
	}

	assertMappedTimeout(t, err, p)
}

// TestMinDeadlineThresholdUnconfiguredDoesNotAbort proves the zero value
// (min_deadline_threshold: unset) behaves as documented on resilienceOptions
// — "unconfigured" — rather than silently inheriting resile.DefaultConfig's
// own 5ms threshold: even a ctx with almost no remaining time still reaches
// dispatch.
func TestMinDeadlineThresholdUnconfiguredDoesNotAbort(t *testing.T) {
	resolver := policy.NewResolver([]config.Policy{
		{ToolPattern: "db_read_*"},
	})
	p, ok := resolver.Resolve("db_read_users")
	if !ok {
		t.Fatal("Resolve should have matched db_read_*")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	dispatched := false
	dispatch := func(context.Context) (*struct{}, error) {
		dispatched = true
		return &struct{}{}, nil
	}

	if _, err := resile.Do(ctx, dispatch, resilienceOptions(p, "db_read_users", metrics.New())...); err != nil {
		t.Fatalf("resile.Do: %v, want it to succeed", err)
	}
	if !dispatched {
		t.Error("dispatch never ran: an unconfigured min_deadline_threshold aborted the call anyway")
	}
}

// TestPanicRecoveryReturnsCleanError proves the FEATURE-016 acceptance
// criterion directly against the exact composition callTool uses
// (baseResilienceOptions, applied to every dispatch regardless of whether a
// policy matches): a panic inside dispatch is recovered by
// resile.WithPanicRecovery rather than crashing the test process, and
// mapResilienceError turns the resulting *resile.PanicError into a clean
// -32603 "Internal error" — not the raw stack trace PanicError.Error()
// would otherwise leak to the client.
//
// This is exercised at the resile.Do/mapResilienceError level rather than
// through a real backend, because nothing in mcp-resile's own dispatch path
// can be made to panic externally without deliberately injecting a bug: a
// panicking backend tool handler is recovered by the backend's own net/http
// server before it ever reaches us, and reaches the gateway only as an
// ordinary connection error.
func TestPanicRecoveryReturnsCleanError(t *testing.T) {
	dispatch := func(context.Context) (*struct{}, error) {
		panic("simulated panic inside dispatch")
	}

	_, err := resile.Do(context.Background(), dispatch, baseResilienceOptions("db_read_users", metrics.New())...)
	if err == nil {
		t.Fatal("resile.Do returned nil error for a panicking dispatch")
	}

	var panicErr *resile.PanicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("resile.Do error = %v (%T), want a *resile.PanicError (i.e. the panic was recovered)", err, err)
	}

	mapped := mapResilienceError(err, nil)
	var rpcErr *jsonrpc.Error
	if !errors.As(mapped, &rpcErr) {
		t.Fatalf("mapResilienceError(%v) = %v, want a *jsonrpc.Error", err, mapped)
	}
	if rpcErr.Code != codeInternalError {
		t.Errorf("Code = %d, want %d", rpcErr.Code, codeInternalError)
	}
	if rpcErr.Message != "Internal error" {
		t.Errorf("Message = %q, want %q", rpcErr.Message, "Internal error")
	}
	if got := mapped.Error(); got == "" || got == panicErr.Error() {
		t.Errorf("mapped error text = %q, want it distinct from the raw panic/stack trace", got)
	}
}

func assertMappedTimeout(t *testing.T, err error, p *policy.Policy) {
	t.Helper()

	mapped := mapResilienceError(err, p.Resilience.RateLimit)
	var rpcErr *jsonrpc.Error
	if !errors.As(mapped, &rpcErr) {
		t.Fatalf("mapResilienceError(%v) = %v, want a *jsonrpc.Error", err, mapped)
	}
	if rpcErr.Code != codeExecutionTimeout {
		t.Errorf("Code = %d, want %d", rpcErr.Code, codeExecutionTimeout)
	}
}
