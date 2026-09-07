package policy_test

import (
	"testing"
	"time"

	"github.com/cinar/mcp-resile/internal/config"
	"github.com/cinar/mcp-resile/internal/policy"
)

func TestResolveNoPolicies(t *testing.T) {
	r := policy.NewResolver(nil)

	if _, ok := r.Resolve("db_read_users"); ok {
		t.Error("Resolve with no policies configured should return ok=false")
	}
}

func TestResolveNoMatch(t *testing.T) {
	r := policy.NewResolver([]config.Policy{
		{ToolPattern: "db_read_*"},
	})

	if _, ok := r.Resolve("http_fetch"); ok {
		t.Error("Resolve for a name matching no pattern should return ok=false")
	}
}

func TestResolveSingleMatch(t *testing.T) {
	r := policy.NewResolver([]config.Policy{
		{ToolPattern: "db_read_*"},
	})

	got, ok := r.Resolve("db_read_users")
	if !ok {
		t.Fatal("Resolve should have matched db_read_*")
	}
	if got.ToolPattern != "db_read_*" {
		t.Errorf("Resolve returned ToolPattern %q, want %q", got.ToolPattern, "db_read_*")
	}
}

// TestResolveOverlapFirstMatchWins pins down the precedence rule the
// FEATURE-010 acceptance criterion requires be unit-tested: when more than
// one pattern matches the same tool name, the one listed first in the
// policies: list wins, regardless of which pattern is more specific.
func TestResolveOverlapFirstMatchWins(t *testing.T) {
	broad := config.Policy{ToolPattern: "db_*"}
	specific := config.Policy{ToolPattern: "db_read_*"}

	broadFirst := policy.NewResolver([]config.Policy{broad, specific})
	got, ok := broadFirst.Resolve("db_read_users")
	if !ok || got.ToolPattern != broad.ToolPattern {
		t.Errorf("broad-first: Resolve = %+v, %v; want ToolPattern %q, true", got, ok, broad.ToolPattern)
	}

	specificFirst := policy.NewResolver([]config.Policy{specific, broad})
	got, ok = specificFirst.Resolve("db_read_users")
	if !ok || got.ToolPattern != specific.ToolPattern {
		t.Errorf("specific-first: Resolve = %+v, %v; want ToolPattern %q, true", got, ok, specific.ToolPattern)
	}
}

func TestResolveNonOverlappingPatterns(t *testing.T) {
	read := config.Policy{ToolPattern: "db_read_*"}
	write := config.Policy{ToolPattern: "db_write_*"}
	r := policy.NewResolver([]config.Policy{read, write})

	if got, ok := r.Resolve("db_write_users"); !ok || got.ToolPattern != write.ToolPattern {
		t.Errorf("Resolve(db_write_users) = %+v, %v; want ToolPattern %q, true", got, ok, write.ToolPattern)
	}
	if got, ok := r.Resolve("db_read_users"); !ok || got.ToolPattern != read.ToolPattern {
		t.Errorf("Resolve(db_read_users) = %+v, %v; want ToolPattern %q, true", got, ok, read.ToolPattern)
	}
}

func TestResolveGlobCharacterClass(t *testing.T) {
	r := policy.NewResolver([]config.Policy{
		{ToolPattern: "db_[rw]ead_users"},
	})

	if _, ok := r.Resolve("db_read_users"); !ok {
		t.Error("Resolve should match db_[rw]ead_users against db_read_users")
	}
	if _, ok := r.Resolve("db_xead_users"); ok {
		t.Error("Resolve should not match db_[rw]ead_users against db_xead_users")
	}
}

func TestResolveCatchAllListedLast(t *testing.T) {
	specific := config.Policy{ToolPattern: "db_read_*"}
	catchAll := config.Policy{ToolPattern: "*"}
	r := policy.NewResolver([]config.Policy{specific, catchAll})

	if got, ok := r.Resolve("db_read_users"); !ok || got.ToolPattern != specific.ToolPattern {
		t.Errorf("Resolve(db_read_users) = %+v, %v; want ToolPattern %q, true", got, ok, specific.ToolPattern)
	}
	if got, ok := r.Resolve("http_fetch"); !ok || got.ToolPattern != catchAll.ToolPattern {
		t.Errorf("Resolve(http_fetch) = %+v, %v; want ToolPattern %q, true", got, ok, catchAll.ToolPattern)
	}
}

// TestResolveSameInstanceAcrossCalls proves the same *Policy (and therefore
// the same underlying *circuit.Breaker) is handed back on every match, since
// a fresh breaker per call could never accumulate enough failures to trip.
func TestResolveSameInstanceAcrossCalls(t *testing.T) {
	r := policy.NewResolver([]config.Policy{
		{ToolPattern: "db_read_*"},
	})

	first, _ := r.Resolve("db_read_users")
	second, _ := r.Resolve("db_read_accounts")
	if first != second {
		t.Error("Resolve returned different *Policy instances for two names matching the same pattern")
	}
}

func TestNoCircuitBreakerWhenUnconfigured(t *testing.T) {
	r := policy.NewResolver([]config.Policy{
		{ToolPattern: "db_read_*"},
	})

	p, ok := r.Resolve("db_read_users")
	if !ok {
		t.Fatal("Resolve should have matched db_read_*")
	}
	if p.CircuitBreaker() != nil {
		t.Error("CircuitBreaker() should be nil when resilience.circuit_breaker is unconfigured")
	}
}

// TestCircuitBreakerTripsAndResets drives the *circuit.Breaker built from a
// policy's resilience.circuit_breaker: config directly (FEATURE-011),
// proving it trips after sustained failures and recovers after reset_timeout.
func TestCircuitBreakerTripsAndResets(t *testing.T) {
	r := policy.NewResolver([]config.Policy{
		{
			ToolPattern: "db_write_*",
			Resilience: config.Resilience{
				CircuitBreaker: &config.CircuitBreaker{
					FailureRate:    50,
					WindowDuration: config.Duration(time.Minute),
					ResetTimeout:   config.Duration(20 * time.Millisecond),
				},
			},
		},
	})

	p, ok := r.Resolve("db_write_users")
	if !ok {
		t.Fatal("Resolve should have matched db_write_*")
	}
	cb := p.CircuitBreaker()
	if cb == nil {
		t.Fatal("CircuitBreaker() should be non-nil when resilience.circuit_breaker is configured")
	}

	failing := func() error { return errBackend }

	// resile's circuit.Breaker requires a minimum number of calls (default
	// 10) in the window before it evaluates the failure rate at all.
	var lastErr error
	for i := 0; i < 20; i++ {
		lastErr = cb.Execute(t.Context(), failing)
	}
	if lastErr == errBackend {
		t.Fatalf("after 20 straight failures the breaker should have opened, last error was still the backend's own: %v", lastErr)
	}

	time.Sleep(30 * time.Millisecond)

	// Half-open: the next call is let through to probe the backend, and
	// still fails, so the breaker reports the backend's own error again
	// rather than failing fast.
	if err := cb.Execute(t.Context(), failing); err != errBackend {
		t.Errorf("after reset_timeout the breaker should probe the backend (half-open), got %v, want %v", err, errBackend)
	}
}

func TestNoRateLimiterWhenUnconfigured(t *testing.T) {
	r := policy.NewResolver([]config.Policy{
		{ToolPattern: "db_read_*"},
	})

	p, ok := r.Resolve("db_read_users")
	if !ok {
		t.Fatal("Resolve should have matched db_read_*")
	}
	if p.RateLimiter() != nil {
		t.Error("RateLimiter() should be nil when resilience.rate_limit is unconfigured")
	}
}

// TestRateLimiterSharedAcrossCalls drives the *resile.RateLimiter built from
// a policy's resilience.rate_limit: config directly (FEATURE-013), proving
// its token bucket is shared (and therefore actually drains) across every
// call resolving to the same policy, rather than resetting per call.
func TestRateLimiterSharedAcrossCalls(t *testing.T) {
	r := policy.NewResolver([]config.Policy{
		{
			ToolPattern: "db_read_*",
			Resilience: config.Resilience{
				RateLimit: &config.RateLimit{
					Rate:     1,
					Interval: config.Duration(time.Minute),
				},
			},
		},
	})

	p, ok := r.Resolve("db_read_users")
	if !ok {
		t.Fatal("Resolve should have matched db_read_*")
	}
	rl := p.RateLimiter()
	if rl == nil {
		t.Fatal("RateLimiter() should be non-nil when resilience.rate_limit is configured")
	}

	if !rl.Acquire(t.Context()) {
		t.Fatal("first Acquire should succeed: the bucket starts full")
	}

	// A second, unrelated call resolving to the same policy shares the same
	// bucket, which the first Acquire above already drained.
	p2, _ := r.Resolve("db_read_accounts")
	if p2.RateLimiter().Acquire(t.Context()) {
		t.Error("second Acquire should fail: the shared bucket has no tokens left within the 1-minute interval")
	}
}

// BenchmarkResolve measures the "policy match" stage of the hot path
// (spec.md §12 FEATURE-023: "validate → policy match → resile pipeline →
// dispatch"): resolving a tool name against a realistic list of policies —
// several non-matching glob patterns before the one that actually matches,
// so the benchmark reflects path.Match's real cost across the list rather
// than a best-case single-policy resolver.
func BenchmarkResolve(b *testing.B) {
	r := policy.NewResolver([]config.Policy{
		{ToolPattern: "http_*"},
		{ToolPattern: "search_*"},
		{ToolPattern: "cache_*"},
		{ToolPattern: "db_read_*", Resilience: config.Resilience{
			CircuitBreaker: &config.CircuitBreaker{FailureRate: 50, WindowDuration: config.Duration(30 * time.Second)},
			RateLimit:      &config.RateLimit{Rate: 1000, Interval: config.Duration(time.Second)},
		}},
		{ToolPattern: "db_write_*"},
	})

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := r.Resolve("db_read_users"); !ok {
			b.Fatal("Resolve should have matched db_read_*")
		}
	}
}

var errBackend = errBackendError{}

type errBackendError struct{}

func (errBackendError) Error() string { return "backend unavailable" }
