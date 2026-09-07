package policy_test

import (
	"testing"

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
	want := config.Policy{ToolPattern: "db_read_*"}
	r := policy.NewResolver([]config.Policy{want})

	got, ok := r.Resolve("db_read_users")
	if !ok {
		t.Fatal("Resolve should have matched db_read_*")
	}
	if got != want {
		t.Errorf("Resolve returned %+v, want %+v", got, want)
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
	if !ok || got != broad {
		t.Errorf("broad-first: Resolve = %+v, %v; want %+v, true", got, ok, broad)
	}

	specificFirst := policy.NewResolver([]config.Policy{specific, broad})
	got, ok = specificFirst.Resolve("db_read_users")
	if !ok || got != specific {
		t.Errorf("specific-first: Resolve = %+v, %v; want %+v, true", got, ok, specific)
	}
}

func TestResolveNonOverlappingPatterns(t *testing.T) {
	read := config.Policy{ToolPattern: "db_read_*"}
	write := config.Policy{ToolPattern: "db_write_*"}
	r := policy.NewResolver([]config.Policy{read, write})

	if got, ok := r.Resolve("db_write_users"); !ok || got != write {
		t.Errorf("Resolve(db_write_users) = %+v, %v; want %+v, true", got, ok, write)
	}
	if got, ok := r.Resolve("db_read_users"); !ok || got != read {
		t.Errorf("Resolve(db_read_users) = %+v, %v; want %+v, true", got, ok, read)
	}
}

func TestResolveGlobCharacterClass(t *testing.T) {
	want := config.Policy{ToolPattern: "db_[rw]ead_users"}
	r := policy.NewResolver([]config.Policy{want})

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

	if got, ok := r.Resolve("db_read_users"); !ok || got != specific {
		t.Errorf("Resolve(db_read_users) = %+v, %v; want %+v, true", got, ok, specific)
	}
	if got, ok := r.Resolve("http_fetch"); !ok || got != catchAll {
		t.Errorf("Resolve(http_fetch) = %+v, %v; want %+v, true", got, ok, catchAll)
	}
}
