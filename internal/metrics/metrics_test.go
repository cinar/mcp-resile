package metrics_test

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cinar/resile"
	"github.com/cinar/resile/circuit"

	"github.com/cinar/mcp-resile/internal/metrics"
)

// scrape renders m's Prometheus exposition format as a string, for tests to
// assert on directly rather than reaching into unexported collector fields —
// the same black-box approach the rest of the suite uses for proving
// observable behavior.
func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("reading /metrics response: %v", err)
	}
	return string(body)
}

// TestRecordRequestExposesCountAndLatency proves the FEATURE-020 acceptance
// criterion's "request count/latency by tool": two RecordRequest calls for
// the same tool/outcome accumulate into one counter series, and the
// histogram's tool label is present.
func TestRecordRequestExposesCountAndLatency(t *testing.T) {
	m := metrics.New()
	m.RecordRequest("db_read_users", "success", 10*time.Millisecond)
	m.RecordRequest("db_read_users", "success", 20*time.Millisecond)
	m.RecordRequest("db_read_users", "rate_limited", 1*time.Millisecond)

	body := scrape(t, m)

	if !strings.Contains(body, `mcp_resile_requests_total{outcome="success",tool="db_read_users"} 2`) {
		t.Errorf("missing/wrong success count in:\n%s", body)
	}
	if !strings.Contains(body, `mcp_resile_requests_total{outcome="rate_limited",tool="db_read_users"} 1`) {
		t.Errorf("missing/wrong rate_limited count in:\n%s", body)
	}
	if !strings.Contains(body, `mcp_resile_request_duration_seconds_count{tool="db_read_users"} 3`) {
		t.Errorf("missing/wrong latency observation count in:\n%s", body)
	}
}

// TestInstrumenterCountsRetriesPastFirstAttempt proves the retry-count
// metric only counts attempts beyond the first, matching resile's own
// 0-indexed RetryState.Attempt convention: a call that succeeds on its first
// try was never retried.
func TestInstrumenterCountsRetriesPastFirstAttempt(t *testing.T) {
	m := metrics.New()
	instr := m.Instrumenter()

	instr.AfterAttempt(context.Background(), resile.RetryState{Name: "db_write_orders", Attempt: 0})
	instr.AfterAttempt(context.Background(), resile.RetryState{Name: "db_write_orders", Attempt: 1})
	instr.AfterAttempt(context.Background(), resile.RetryState{Name: "db_write_orders", Attempt: 2})

	body := scrape(t, m)
	if !strings.Contains(body, `mcp_resile_retries_total{tool="db_write_orders"} 2`) {
		t.Errorf("missing/wrong retry count in:\n%s", body)
	}
}

// TestInstrumenterCountsRateLimitRejections proves OnRateLimitExceeded
// (resile's own hook for exactly this event) feeds the rate-limit-rejection
// metric, labeled by the tool the rejected call was for.
func TestInstrumenterCountsRateLimitRejections(t *testing.T) {
	m := metrics.New()
	instr := m.Instrumenter()

	instr.OnRateLimitExceeded(context.Background(), resile.RetryState{Name: "db_write_orders"})
	instr.OnRateLimitExceeded(context.Background(), resile.RetryState{Name: "db_write_orders"})

	body := scrape(t, m)
	if !strings.Contains(body, `mcp_resile_rate_limit_rejections_total{tool="db_write_orders"} 2`) {
		t.Errorf("missing/wrong rate-limit rejection count in:\n%s", body)
	}
}

// TestWatchCircuitBreakerRecordsTransitions proves WatchCircuitBreaker
// observes a real circuit.Breaker's own state machine (via its Health()
// event channel) rather than mcp-resile re-deriving open/closed state
// itself: forcing a real breaker open produces both a transition count and
// an updated current-state gauge.
func TestWatchCircuitBreakerRecordsTransitions(t *testing.T) {
	m := metrics.New()

	cb := circuit.New(circuit.Config{
		FailureRateThreshold: 50,
		MinimumCalls:         1,
		ResetTimeout:         time.Minute,
	})
	m.WatchCircuitBreaker("db_write_*", cb)

	failing := errors.New("simulated backend failure")
	if err := cb.Execute(context.Background(), func() error { return failing }); !errors.Is(err, failing) {
		t.Fatalf("cb.Execute: %v, want %v", err, failing)
	}

	deadline := time.Now().Add(time.Second)
	for {
		body := scrape(t, m)
		gotTransition := strings.Contains(body, `mcp_resile_circuit_breaker_transitions_total{state="open",tool_pattern="db_write_*"} 1`)
		gotState := strings.Contains(body, `mcp_resile_circuit_breaker_state{tool_pattern="db_write_*"} 2`)
		if gotTransition && gotState {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for circuit-open metrics; last scrape:\n%s", body)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestWatchCircuitBreakerStartsClosed proves a freshly watched breaker's
// gauge reads 0 (closed) immediately, before any call has ever gone through
// it — matching circuit.New's own StateClosed default.
func TestWatchCircuitBreakerStartsClosed(t *testing.T) {
	m := metrics.New()
	cb := circuit.New(circuit.Config{})
	m.WatchCircuitBreaker("db_read_*", cb)

	body := scrape(t, m)
	if !strings.Contains(body, `mcp_resile_circuit_breaker_state{tool_pattern="db_read_*"} 0`) {
		t.Errorf("missing/wrong initial state gauge in:\n%s", body)
	}
}
