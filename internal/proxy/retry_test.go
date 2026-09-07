package proxy

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

func TestIsTransientErrorNil(t *testing.T) {
	if isTransientError(nil) {
		t.Error("isTransientError(nil) = true, want false")
	}
}

func TestIsTransientErrorTransportRejected(t *testing.T) {
	// Mirrors how go-sdk's streamable client actually wraps a 502/503/504
	// response: fmt.Errorf("%w: ...", jsonrpc2.ErrRejected, ...).
	err := fmt.Errorf("%w: POST /mcp: Service Unavailable", &jsonrpc.Error{Code: -32005, Message: "rejected by transport"})
	if !isTransientError(err) {
		t.Errorf("isTransientError(%v) = false, want true (wraps a transport-rejected status)", err)
	}
}

func TestIsTransientErrorNetworkFailure(t *testing.T) {
	err := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	if !isTransientError(err) {
		t.Errorf("isTransientError(%v) = false, want true (a net.Error)", err)
	}
}

func TestIsTransientErrorApplicationLevel(t *testing.T) {
	// An unknown-tool or invalid-params style error from the backend: a
	// real JSON-RPC error, but not the transport-rejected one, so it must
	// never be treated as transient.
	err := &jsonrpc.Error{Code: -32601, Message: "unknown tool"}
	if isTransientError(err) {
		t.Errorf("isTransientError(%v) = true, want false (an application-level error, not a transport rejection)", err)
	}
}

func TestIsTransientErrorGeneric(t *testing.T) {
	if isTransientError(errors.New("boom")) {
		t.Error("isTransientError(generic error) = true, want false")
	}
}
