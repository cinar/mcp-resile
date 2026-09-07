package proxy

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func textSchema(required bool) any {
	props := map[string]any{"text": map[string]any{"type": "string"}}
	schema := map[string]any{"type": "object", "properties": props}
	if required {
		schema["required"] = []string{"text"}
	}
	return schema
}

func TestSchemaCacheRefreshAndGet(t *testing.T) {
	c := newSchemaCache()

	if _, ok := c.get("echo"); ok {
		t.Fatal("get on an empty cache returned ok=true")
	}

	if err := c.refresh("echo", &mcp.Tool{Name: "echo", InputSchema: textSchema(true)}); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	resolved, ok := c.get("echo")
	if !ok {
		t.Fatal("get after refresh returned ok=false")
	}
	if resolved == nil {
		t.Fatal("get after refresh returned a nil *jsonschema.Resolved")
	}
}

// TestSchemaCacheRefreshNilInputSchemaIsNoop proves a tool with no declared
// inputSchema (per MCP: "any input is valid") is never cached, so a later
// lookup correctly finds no schema to enforce rather than an empty one that
// would reject every argument.
func TestSchemaCacheRefreshNilInputSchemaIsNoop(t *testing.T) {
	c := newSchemaCache()

	if err := c.refresh("anything", &mcp.Tool{Name: "anything", InputSchema: nil}); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if _, ok := c.get("anything"); ok {
		t.Error("get after refreshing a tool with a nil InputSchema returned ok=true")
	}
}

// TestSchemaCacheRefreshMalformedSchemaFailsClosedOnCaching proves a
// malformed inputSchema (a backend's own bug) returns an error from
// refresh and is not cached, rather than caching something unusable or
// panicking.
func TestSchemaCacheRefreshMalformedSchemaFailsClosedOnCaching(t *testing.T) {
	c := newSchemaCache()

	err := c.refresh("bad", &mcp.Tool{Name: "bad", InputSchema: map[string]any{"type": 123}})
	if err == nil {
		t.Fatal("refresh: got nil error for a malformed inputSchema")
	}

	if _, ok := c.get("bad"); ok {
		t.Error("get after a failed refresh returned ok=true")
	}
}

func TestValidateArgumentsAcceptsValid(t *testing.T) {
	resolved, err := compileSchema(textSchema(true))
	if err != nil {
		t.Fatalf("compileSchema: %v", err)
	}

	if err := validateArguments(resolved, "echo", json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Errorf("validateArguments: %v, want it to accept valid arguments", err)
	}
}

func TestValidateArgumentsRejectsTypeMismatch(t *testing.T) {
	resolved, err := compileSchema(textSchema(true))
	if err != nil {
		t.Fatalf("compileSchema: %v", err)
	}

	err = validateArguments(resolved, "echo", json.RawMessage(`{"text":123}`))
	assertInvalidParams(t, err, "echo")
}

func TestValidateArgumentsRejectsMissingRequired(t *testing.T) {
	resolved, err := compileSchema(textSchema(true))
	if err != nil {
		t.Fatalf("compileSchema: %v", err)
	}

	err = validateArguments(resolved, "echo", json.RawMessage(`{}`))
	assertInvalidParams(t, err, "echo")
}

func TestValidateArgumentsRejectsMalformedJSON(t *testing.T) {
	resolved, err := compileSchema(textSchema(true))
	if err != nil {
		t.Fatalf("compileSchema: %v", err)
	}

	err = validateArguments(resolved, "echo", json.RawMessage(`{not json`))
	assertInvalidParams(t, err, "echo")
}

// TestValidateArgumentsEmptyArgumentsIsAnEmptyObject proves omitted
// arguments (an empty raw message, as a client sends when a tool takes no
// input) validate as {} rather than as JSON null, so a schema with no
// required properties still accepts them.
func TestValidateArgumentsEmptyArgumentsIsAnEmptyObject(t *testing.T) {
	resolved, err := compileSchema(textSchema(false))
	if err != nil {
		t.Fatalf("compileSchema: %v", err)
	}

	if err := validateArguments(resolved, "echo", nil); err != nil {
		t.Errorf("validateArguments(nil): %v, want it to accept an implicit empty object", err)
	}
}

// BenchmarkValidateArguments measures the "validate" stage of the hot path
// (spec.md §12 FEATURE-023: "validate → policy match → resile pipeline →
// dispatch"), against an already-compiled/cached schema — the case every
// tools/call after the first hits, per schemaCache's whole point
// (FEATURE-017).
func BenchmarkValidateArguments(b *testing.B) {
	resolved, err := compileSchema(textSchema(true))
	if err != nil {
		b.Fatalf("compileSchema: %v", err)
	}
	args := json.RawMessage(`{"text":"hello, world"}`)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := validateArguments(resolved, "echo", args); err != nil {
			b.Fatalf("validateArguments: %v", err)
		}
	}
}

func assertInvalidParams(t *testing.T, err error, wantTool string) {
	t.Helper()

	if err == nil {
		t.Fatal("got nil error, want a schema violation")
	}
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v (%T), want a *jsonrpc.Error", err, err)
	}
	if rpcErr.Code != codeInvalidParams {
		t.Errorf("Code = %d, want %d", rpcErr.Code, codeInvalidParams)
	}
	if rpcErr.Message != "Invalid params" {
		t.Errorf("Message = %q, want %q", rpcErr.Message, "Invalid params")
	}

	var data invalidParamsData
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		t.Fatalf("Data %s did not unmarshal: %v", rpcErr.Data, err)
	}
	if data.Tool != wantTool {
		t.Errorf("Data.Tool = %q, want %q", data.Tool, wantTool)
	}
	if data.Detail == "" {
		t.Error("Data.Detail is empty, want a field-level validation message")
	}
}
