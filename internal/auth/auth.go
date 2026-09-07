// Package auth enforces the gateway's bearer token/API key allow-list on
// every ingress HTTP request, per spec.md §5.4 and the FEATURE-019
// acceptance criterion: a request without a valid token is rejected before
// it ever reaches MCP session handling, let alone a backend.
package auth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/cinar/mcp-resile/internal/config"
)

// codeUnauthorized reuses JSON-RPC's standard "Method not found" code for
// an unauthorized request, per spec.md §6's "Unauthorized Access" row and
// CLAUDE.md's guidance to use -32601 for auth errors: it deliberately looks
// identical to an unknown method to an unauthorized caller, hiding that the
// gateway (or any tool behind it) exists at all rather than confirming it's
// there but off-limits.
const codeUnauthorized = -32601

// Middleware wraps next with cfg's bearer/API-key check. If cfg.Enabled is
// false it returns next unchanged — auth is opt-in, per spec.md §7's
// auth.enabled field. Otherwise, every request's cfg.Header value (with a
// leading "Bearer " scheme stripped, if present, for the Authorization
// header's usual convention) is compared against cfg.Tokens using a
// constant-time comparison, so response timing can't be used to probe
// valid tokens; anything not on the list gets codeUnauthorized before next
// ever runs.
func Middleware(cfg config.Auth, next http.Handler) http.Handler {
	if !cfg.Enabled {
		return next
	}

	tokens := make([][]byte, len(cfg.Tokens))
	for i, t := range cfg.Tokens {
		tokens[i] = []byte(t)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := []byte(extractToken(r.Header.Get(cfg.Header)))
		if len(token) == 0 || !isAllowed(token, tokens) {
			writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractToken strips a case-insensitive "Bearer " scheme prefix if
// present, so a token compares correctly whether the header carries
// "Bearer <token>" (the Authorization convention) or an API-key-style
// header whose raw value is the token itself (e.g. X-API-Key).
func extractToken(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return header[len(prefix):]
	}
	return header
}

// isAllowed reports whether token matches one of tokens. Every candidate is
// compared, not just until the first match, so the response time doesn't
// depend on where in the list (or whether at all) a match occurs.
func isAllowed(token []byte, tokens [][]byte) bool {
	ok := false
	for _, candidate := range tokens {
		if subtle.ConstantTimeCompare(token, candidate) == 1 {
			ok = true
		}
	}
	return ok
}

// unauthorizedBody is JSON-RPC 2.0's own shape for an error response whose
// request id couldn't be determined (we reject before ever parsing the
// body): id: null, per the JSON-RPC spec's own guidance for exactly this
// case.
var unauthorizedBody = struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Error   *jsonrpc.Error `json:"error"`
}{
	JSONRPC: "2.0",
	Error:   &jsonrpc.Error{Code: codeUnauthorized, Message: "Method not found"},
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(unauthorizedBody)
}
