package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// codeInvalidParams is JSON-RPC 2.0's own standard "Invalid params" code
// (spec.md §6), reused here rather than one of mcp-resile's own -33000-and-
// below codes: a schema violation is exactly what -32602 exists for.
const codeInvalidParams = -32602

// schemaCache holds compiled, resolved JSON Schemas for tools' inputSchema,
// keyed by the gateway-facing (prefixed) tool name, so a tools/call
// validates against a schema resolved once (FEATURE-017) rather than
// recompiled on every call. It's safe for concurrent use.
type schemaCache struct {
	mu     sync.RWMutex
	byName map[string]*jsonschema.Resolved
}

func newSchemaCache() *schemaCache {
	return &schemaCache{byName: make(map[string]*jsonschema.Resolved)}
}

func (c *schemaCache) get(name string) (*jsonschema.Resolved, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	resolved, ok := c.byName[name]
	return resolved, ok
}

// refresh compiles tool's InputSchema and stores it under name, replacing
// any existing entry — used both when a client's own tools/list response is
// merged (listTools) and when resolveTool below fills a cache miss. A tool
// with no InputSchema accepts any input (per MCP: omitting inputSchema
// means "no constraints"), so nothing is cached for it, and a later lookup
// correctly finds no schema to enforce.
func (c *schemaCache) refresh(name string, tool *mcp.Tool) error {
	if tool.InputSchema == nil {
		return nil
	}
	resolved, err := compileSchema(tool.InputSchema)
	if err != nil {
		return fmt.Errorf("tool %q: compiling inputSchema: %w", name, err)
	}
	c.mu.Lock()
	c.byName[name] = resolved
	c.mu.Unlock()
	return nil
}

// resolveTool returns the compiled schema for the (prefixed) tool name,
// ok=false if the tool declares no inputSchema (or its schema failed to
// compile — a backend's own malformed advertisement can't be turned into a
// boundary to enforce, so that tool's calls fall back to unvalidated rather
// than being blocked by it). On a cache miss — a tools/call that arrived
// without a preceding tools/list ever populating the cache for this backend
// — it fetches tools/list from just that one backend once and caches every
// tool it returns, amortizing the round trip across that backend's whole
// tool set rather than refetching per call.
func (c *schemaCache) resolveTool(ctx context.Context, route Route, fullName string) (*jsonschema.Resolved, bool, error) {
	if resolved, ok := c.get(fullName); ok {
		return resolved, true, nil
	}

	result, err := route.Backend.ListTools(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("resolving inputSchema for %q: %w", fullName, err)
	}
	for _, tool := range result.Tools {
		_ = c.refresh(route.Prefix+tool.Name, tool)
	}

	resolved, ok := c.get(fullName)
	return resolved, ok, nil
}

// compileSchema converts a Tool.InputSchema value — a map[string]any when it
// arrived over the wire from a backend (see Tool.InputSchema's doc comment)
// — into a resolved github.com/google/jsonschema-go Schema ready to
// validate instances against. jsonschema-go is already a transitive
// dependency of go-sdk (it's what AddTool itself uses for locally-defined
// tools), so reusing it here is the zero-reinvention choice rather than
// hand-rolling schema validation.
func compileSchema(raw any) (*jsonschema.Resolved, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshaling inputSchema: %w", err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("unmarshaling inputSchema: %w", err)
	}
	return schema.Resolve(nil)
}

// validateArguments decodes raw (a tools/call request's raw arguments,
// possibly empty when the client omitted them) and validates it against
// resolved, per FEATURE-017. A schema violation is returned as the
// -32602 "Invalid params" JSON-RPC error spec.md §6 defines, carrying the
// validator's own message as field-level detail so the calling LLM can
// self-correct.
func validateArguments(resolved *jsonschema.Resolved, toolName string, raw json.RawMessage) error {
	instance := any(map[string]any{})
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &instance); err != nil {
			return invalidParamsError(toolName, "arguments: invalid JSON: "+err.Error())
		}
	}
	if err := resolved.Validate(instance); err != nil {
		return invalidParamsError(toolName, err.Error())
	}
	return nil
}

type invalidParamsData struct {
	Tool   string `json:"tool"`
	Detail string `json:"detail"`
}

func invalidParamsError(toolName, detail string) error {
	return &jsonrpc.Error{
		Code:    codeInvalidParams,
		Message: "Invalid params",
		Data:    mustMarshal(invalidParamsData{Tool: toolName, Detail: detail}),
	}
}
