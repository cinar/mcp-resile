package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cinar/mcp-resile/internal/auth"
	"github.com/cinar/mcp-resile/internal/config"
)

func passthroughHandler(t *testing.T) (http.Handler, *bool) {
	t.Helper()
	called := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}), &called
}

// TestMiddlewareDisabledIsNoop proves auth.enabled: false (the default)
// never touches a request, matching the "auth is opt-in" contract.
func TestMiddlewareDisabledIsNoop(t *testing.T) {
	next, called := passthroughHandler(t)
	handler := auth.Middleware(config.Auth{Enabled: false}, next)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !*called {
		t.Error("next was never called with auth disabled")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestMiddlewareRejectsMissingToken proves a request with no token on the
// configured header is rejected before next runs (FEATURE-019).
func TestMiddlewareRejectsMissingToken(t *testing.T) {
	next, called := passthroughHandler(t)
	cfg := config.Auth{Enabled: true, Header: "Authorization", Tokens: []string{"secret-1"}}
	handler := auth.Middleware(cfg, next)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if *called {
		t.Error("next ran despite a missing token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	var body struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Error   struct {
			Code    int64  `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
	if body.Error.Code != -32601 {
		t.Errorf("error.code = %d, want -32601", body.Error.Code)
	}
	if body.ID != nil {
		t.Errorf("id = %v, want null (request was rejected before it was parsed)", body.ID)
	}
}

// TestMiddlewareRejectsUnknownToken proves a token not on the allow-list is
// rejected the same way as a missing one, not just an empty header.
func TestMiddlewareRejectsUnknownToken(t *testing.T) {
	next, called := passthroughHandler(t)
	cfg := config.Auth{Enabled: true, Header: "Authorization", Tokens: []string{"secret-1"}}
	handler := auth.Middleware(cfg, next)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer not-the-right-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if *called {
		t.Error("next ran despite an unrecognized token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestMiddlewareAllowsValidBearerToken proves a token on the allow-list,
// sent as "Bearer <token>" on the configured header, passes through
// unaffected.
func TestMiddlewareAllowsValidBearerToken(t *testing.T) {
	next, called := passthroughHandler(t)
	cfg := config.Auth{Enabled: true, Header: "Authorization", Tokens: []string{"secret-1", "secret-2"}}
	handler := auth.Middleware(cfg, next)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer secret-2")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !*called {
		t.Error("next never ran despite a valid token")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestMiddlewareAllowsValidAPIKeyHeader proves a raw API-key-style header
// (no "Bearer " scheme) also works, per spec.md §7's "X-API-Key" example.
func TestMiddlewareAllowsValidAPIKeyHeader(t *testing.T) {
	next, called := passthroughHandler(t)
	cfg := config.Auth{Enabled: true, Header: "X-API-Key", Tokens: []string{"api-key-1"}}
	handler := auth.Middleware(cfg, next)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-API-Key", "api-key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !*called {
		t.Error("next never ran despite a valid API key")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestMiddlewareRejectsBearerPrefixOnRawHeader proves a literal "Bearer "
// prefix isn't accidentally accepted as part of an API-key token: stripping
// only applies, and only ever needs to apply, when it's actually present.
func TestMiddlewareRejectsBearerPrefixMismatch(t *testing.T) {
	next, called := passthroughHandler(t)
	cfg := config.Auth{Enabled: true, Header: "Authorization", Tokens: []string{"secret-1"}}
	handler := auth.Middleware(cfg, next)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "secret-1-with-extra-suffix")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if *called {
		t.Error("next ran despite the header value not exactly matching any configured token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
