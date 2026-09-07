// Package metrics implements FEATURE-020's Prometheus /metrics endpoint:
// request count/latency by tool, circuit breaker state transitions,
// rate-limit rejections, and retry counts.
//
// Per CLAUDE.md's zero-reinvention mandate, this package derives everything
// it can from resile's own instrumentation surface rather than re-deriving
// retry/rate-limit/circuit-breaker state itself: retries and rate-limit
// rejections are counted via resile.Instrumenter (the hook interface resile
// exists specifically to expose these lifecycle events through, see
// Metrics.Instrumenter), and circuit breaker transitions are counted by
// consuming circuit.Breaker's own Health() event channel (see
// Metrics.WatchCircuitBreaker). Only the request count/latency metrics are
// mcp-resile's own instrumentation, since they wrap the proxy's dispatch
// call itself rather than anything resile tracks internally.
package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/cinar/resile"
	"github.com/cinar/resile/circuit"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// healthStateLabel maps resile/circuit's HealthState (Healthy/Degraded/
// Unhealthy) to the circuit's actual state name, since that's what an
// operator reading the metric expects to see, not the more abstract
// health-state vocabulary circuit.Breaker.Health() emits events in.
var healthStateLabel = map[circuit.HealthState]string{
	circuit.HealthStateHealthy:   "closed",
	circuit.HealthStateDegraded:  "half_open",
	circuit.HealthStateUnhealthy: "open",
}

// Metrics holds every Prometheus collector the gateway exposes.
type Metrics struct {
	registry *prometheus.Registry

	requestsTotal       *prometheus.CounterVec
	requestDuration     *prometheus.HistogramVec
	retriesTotal        *prometheus.CounterVec
	rateLimitRejections *prometheus.CounterVec
	circuitTransitions  *prometheus.CounterVec
	circuitState        *prometheus.GaugeVec
}

// New returns a Metrics with every collector registered on its own
// registry, so /metrics only ever exposes mcp-resile's own series, not
// whatever else might be registered on prometheus's global DefaultRegisterer
// in-process.
func New() *Metrics {
	registry := prometheus.NewRegistry()
	factory := promauto.With(registry)

	return &Metrics{
		registry: registry,

		requestsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_resile_requests_total",
			Help: "Total tools/call dispatches, by tool and outcome.",
		}, []string{"tool", "outcome"}),

		requestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mcp_resile_request_duration_seconds",
			Help:    "tools/call dispatch latency, by tool.",
			Buckets: prometheus.DefBuckets,
		}, []string{"tool"}),

		retriesTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_resile_retries_total",
			Help: "Retry attempts (beyond the first) resile issued, by tool.",
		}, []string{"tool"}),

		rateLimitRejections: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_resile_rate_limit_rejections_total",
			Help: "Requests rejected by a policy's rate limiter, by tool.",
		}, []string{"tool"}),

		circuitTransitions: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_resile_circuit_breaker_transitions_total",
			Help: "Circuit breaker state transitions, by tool_pattern and the state entered.",
		}, []string{"tool_pattern", "state"}),

		circuitState: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mcp_resile_circuit_breaker_state",
			Help: "Current circuit breaker state (0=closed, 1=half_open, 2=open), by tool_pattern.",
		}, []string{"tool_pattern"}),
	}
}

// Handler returns the http.Handler that serves /metrics in the Prometheus
// text exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// RecordRequest records one tools/call dispatch's outcome and latency,
// keyed by its full (prefixed) tool name.
func (m *Metrics) RecordRequest(tool, outcome string, duration time.Duration) {
	m.requestsTotal.WithLabelValues(tool, outcome).Inc()
	m.requestDuration.WithLabelValues(tool).Observe(duration.Seconds())
}

// Instrumenter returns a resile.Instrumenter wired to this Metrics: it
// counts a retry every time resile re-attempts a dispatch (RetryState.Attempt
// > 0, i.e. every AfterAttempt call past the first), and counts a rate-limit
// rejection every time resile's rate limiter middleware rejects a request
// before it ever reaches the retry loop. Pass it to resile.Do via
// resile.WithInstrumenter, alongside resile.WithName(tool) so RetryState.Name
// identifies which tool the event belongs to.
func (m *Metrics) Instrumenter() resile.Instrumenter {
	return instrumenter{m: m}
}

type instrumenter struct {
	m *Metrics
}

func (i instrumenter) BeforeAttempt(ctx context.Context, _ resile.RetryState) context.Context {
	return ctx
}

func (i instrumenter) AfterAttempt(_ context.Context, state resile.RetryState) {
	if state.Attempt > 0 {
		i.m.retriesTotal.WithLabelValues(state.Name).Inc()
	}
}

func (i instrumenter) OnBulkheadFull(_ context.Context, _ resile.RetryState) {
	// V1 doesn't configure bulkheads (spec.md §11), so this never fires;
	// it exists only to satisfy resile.Instrumenter.
}

func (i instrumenter) OnRateLimitExceeded(_ context.Context, state resile.RetryState) {
	i.m.rateLimitRejections.WithLabelValues(state.Name).Inc()
}

// WatchCircuitBreaker consumes cb's Health() event channel for the lifetime
// of the process, updating the circuit breaker transition/state metrics for
// toolPattern (the policy's tool_pattern, since that's the only stable label
// available — a breaker is shared by every tool name the pattern matches,
// not one per call). It's meant to be called once per configured circuit
// breaker, right after the policy resolver is built; the goroutine it starts
// runs until the process exits, which is fine since policies (and their
// breakers) live for the whole process lifetime in v1 — there's no dynamic
// reconfiguration to unwind it for.
func (m *Metrics) WatchCircuitBreaker(toolPattern string, cb *circuit.Breaker) {
	m.circuitState.WithLabelValues(toolPattern).Set(0) // starts Closed, per circuit.New

	// Health() lazily creates cb's event channel on first call; it must be
	// created here, synchronously, before returning — not inside the
	// goroutine below — or a state transition racing the goroutine's first
	// scheduling would find the channel not yet initialized and be silently
	// dropped (circuit.Breaker.emitStateEvent is a no-op until Health() has
	// been called once).
	events := cb.Health()

	go func() {
		for event := range events {
			state, ok := healthStateLabel[event.State]
			if !ok {
				continue
			}
			m.circuitTransitions.WithLabelValues(toolPattern, state).Inc()
			m.circuitState.WithLabelValues(toolPattern).Set(circuitStateValue(state))
		}
	}()
}

func circuitStateValue(state string) float64 {
	switch state {
	case "half_open":
		return 1
	case "open":
		return 2
	default:
		return 0
	}
}
