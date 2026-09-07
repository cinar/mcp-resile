// Package policy resolves a tool name to the policy that governs it, per
// spec.md §7 and the FEATURE-010 acceptance criterion: a tool name resolves
// to exactly one matching policy (or a documented default) before dispatch.
//
// Matching is by glob (path.Match) against each policy's tool_pattern,
// first-match-wins in the order policies are listed in mcp-resile.yaml —
// so when two patterns overlap for the same tool name, the one listed
// first takes precedence, regardless of which is more specific. This is
// purely a resolution engine: it doesn't itself enforce anything. Callers
// (FEATURE-011 onward) use the resolved config.Policy's validation/
// resilience fields to drive the actual behavior.
package policy

import (
	"path"

	"github.com/cinar/mcp-resile/internal/config"
)

// Resolver resolves a tool name to the config.Policy that governs it.
type Resolver struct {
	policies []config.Policy
}

// NewResolver builds a Resolver from the policies: list, preserving its
// order for first-match-wins precedence.
func NewResolver(policies []config.Policy) *Resolver {
	return &Resolver{policies: policies}
}

// Resolve returns the first policy in config order whose tool_pattern
// matches toolName, and true. If none matches — including when no policies
// are configured at all — it returns the zero config.Policy and false: the
// documented default is that no policy applies, and callers proceed
// without any policy-driven validation or resilience behavior.
func (r *Resolver) Resolve(toolName string) (config.Policy, bool) {
	for _, p := range r.policies {
		if matched, _ := path.Match(p.ToolPattern, toolName); matched {
			return p, true
		}
	}
	return config.Policy{}, false
}
