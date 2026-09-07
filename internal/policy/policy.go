// Package policy resolves a tool name to the policy that governs it, per
// spec.md §7 and the FEATURE-010 acceptance criterion: a tool name resolves
// to exactly one matching policy (or a documented default) before dispatch.
//
// Matching is by glob (path.Match) against each policy's tool_pattern,
// first-match-wins in the order policies are listed in mcp-resile.yaml —
// so when two patterns overlap for the same tool name, the one listed
// first takes precedence, regardless of which is more specific. This is
// purely a resolution engine: it doesn't itself enforce anything. Callers
// use the resolved *Policy's resile primitives (FEATURE-011 onward) to
// drive the actual behavior.
package policy

import (
	"path"
	"time"

	"github.com/cinar/resile/circuit"

	"github.com/cinar/mcp-resile/internal/config"
)

// Policy pairs one config.Policy with the resile primitives it configures.
// A primitive such as a circuit breaker holds state that must persist
// across every call matching this policy, so it's built once, here, rather
// than fresh per call — Resolver hands out the same *Policy (and therefore
// the same underlying *circuit.Breaker) every time a tool name matches it.
type Policy struct {
	config.Policy

	circuitBreaker *circuit.Breaker
}

// CircuitBreaker returns the policy's circuit breaker, or nil if its
// resilience.circuit_breaker: block wasn't configured.
func (p *Policy) CircuitBreaker() *circuit.Breaker {
	return p.circuitBreaker
}

// newPolicy builds the runtime Policy for one config.Policy, constructing
// its resile primitives up front.
func newPolicy(cfg config.Policy) *Policy {
	p := &Policy{Policy: cfg}

	if cb := cfg.Resilience.CircuitBreaker; cb != nil {
		p.circuitBreaker = circuit.New(circuit.Config{
			// WindowTimeBased matches spec.md §7's window_duration field:
			// the failure rate is measured over a trailing time window, not
			// over the last N calls (circuit.WindowCountBased, the
			// package's own default).
			WindowType:           circuit.WindowTimeBased,
			WindowDuration:       time.Duration(cb.WindowDuration),
			FailureRateThreshold: cb.FailureRate,
			ResetTimeout:         time.Duration(cb.ResetTimeout),
		})
	}

	return p
}

// Resolver resolves a tool name to the *Policy that governs it.
type Resolver struct {
	policies []*Policy
}

// NewResolver builds a Resolver from the policies: list, preserving its
// order for first-match-wins precedence, and constructing each policy's
// resile primitives once up front.
func NewResolver(policies []config.Policy) *Resolver {
	built := make([]*Policy, len(policies))
	for i, p := range policies {
		built[i] = newPolicy(p)
	}
	return &Resolver{policies: built}
}

// Resolve returns the first policy in config order whose tool_pattern
// matches toolName, and true. If none matches — including when no policies
// are configured at all — it returns nil and false: the documented default
// is that no policy applies, and callers proceed without any policy-driven
// validation or resilience behavior.
func (r *Resolver) Resolve(toolName string) (*Policy, bool) {
	for _, p := range r.policies {
		if matched, _ := path.Match(p.ToolPattern, toolName); matched {
			return p, true
		}
	}
	return nil, false
}
