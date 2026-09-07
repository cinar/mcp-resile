package proxy

import (
	"errors"
	"net"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// errTransientBackendStatus matches go-sdk's own internal
// jsonrpc2.ErrRejected (code -32005, "rejected by transport"), which its
// streamable HTTP client wraps around 500/502/503/504/429 backend
// responses specifically because those are meant to be retried rather than
// treated as a broken connection. jsonrpc.Error is a public alias for the
// same jsonrpc2.WireError type, whose Is method compares by Code alone, so
// errors.Is against this value reuses go-sdk's own transient/non-transient
// classification instead of mcp-resile re-parsing HTTP status codes itself.
var errTransientBackendStatus = &jsonrpc.Error{Code: -32005}

// isTransientError reports whether err looks like a transient backend
// failure safe to retry (FEATURE-012, spec.md §12: "connection reset,
// 502/503/504"): a 500/502/503/504/429 response from the backend (see
// errTransientBackendStatus above), or a network-level failure such as a
// dial error, connection reset, or timeout (any error implementing
// net.Error, which *net.OpError and its relatives satisfy). Anything else
// — including a well-formed application-level JSON-RPC error from the
// backend, e.g. an unknown-tool or invalid-params error — is treated as
// non-transient and is never retried.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errTransientBackendStatus) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
