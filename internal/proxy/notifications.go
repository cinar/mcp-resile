package proxy

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// progressCleanupDelay bounds how long a completed call's progress-token
// mapping is kept around before being forgotten. It can't be zero: the
// backend's final notifications/progress for a call and its tools/call
// response are two separate messages, dispatched independently on the
// client side, so the notification can still be in flight (or its handler
// not yet scheduled) at the moment the response arrives and the call
// returns. Deleting the mapping synchronously on return is a real race, not
// a hypothetical one; this delay is comfortably longer than any such
// in-flight gap while still bounding memory for a long-running gateway.
const progressCleanupDelay = 5 * time.Second

// NotificationRouter forwards backend-initiated notifications
// (notifications/progress, notifications/message) to the gateway's clients,
// per spec.md §5.1 (FEATURE-009). notifications/cancelled (client→backend)
// needs no glue here: the SDK propagates context cancellation from an
// incoming request straight through to the outgoing backend call that
// shares its context, which is exactly what Middleware's callTool does.
//
// A single backend connection is shared by every client session proxied
// through it (see Route), so progress notifications can't be routed by
// their wire token alone: two different clients may legally choose the same
// token independently. TrackProgress rewrites the token the gateway sends
// to the backend to one unique to this in-flight call, so the response can
// be routed back to the right client and restored to the client's original
// token.
type NotificationRouter struct {
	mu       sync.Mutex
	server   *mcp.Server
	next     uint64
	inFlight map[string]progressTarget
}

type progressTarget struct {
	session       *mcp.ServerSession
	originalToken any
}

// NewNotificationRouter creates a router with no attached server; Attach
// must be called before any notifications/message can be broadcast (there
// is nowhere to broadcast progress notifications, that part works
// regardless, since each is routed to a specific session recorded by
// TrackProgress rather than to every connected session).
func NewNotificationRouter() *NotificationRouter {
	return &NotificationRouter{inFlight: make(map[string]progressTarget)}
}

// Attach records the gateway's own ingress server, whose currently
// connected sessions are the destination for backend log broadcasts. It
// must be called once, before the server starts accepting connections.
func (r *NotificationRouter) Attach(server *mcp.Server) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.server = server
}

// TrackProgress registers session as the destination for progress
// notifications carrying the returned synthetic token, and returns a
// cleanup func the caller must run once the backend call this token was
// issued for has completed. If originalToken is nil (the client requested
// no progress updates), it returns an empty token and a no-op cleanup.
func (r *NotificationRouter) TrackProgress(session *mcp.ServerSession, originalToken any) (token string, done func()) {
	if originalToken == nil {
		return "", func() {}
	}

	r.mu.Lock()
	r.next++
	token = fmt.Sprintf("mcp-resile-%d", r.next)
	r.inFlight[token] = progressTarget{session: session, originalToken: originalToken}
	r.mu.Unlock()

	return token, func() {
		time.AfterFunc(progressCleanupDelay, func() {
			r.mu.Lock()
			delete(r.inFlight, token)
			r.mu.Unlock()
		})
	}
}

// HandleProgress is a mcp.ClientOptions.ProgressNotificationHandler that
// resolves a backend's notifications/progress back to the client session
// that issued the originating call, restoring the client's own progress
// token before forwarding.
func (r *NotificationRouter) HandleProgress(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
	token, ok := req.Params.ProgressToken.(string)
	if !ok {
		return
	}

	r.mu.Lock()
	target, ok := r.inFlight[token]
	r.mu.Unlock()
	if !ok {
		return
	}

	restored := *req.Params
	restored.ProgressToken = target.originalToken
	if err := target.session.NotifyProgress(ctx, &restored); err != nil {
		log.Printf("proxy: forwarding notifications/progress to client: %v", err)
	}
}

// HandleLog is a mcp.ClientOptions.LoggingMessageHandler that broadcasts a
// backend's notifications/message to every client currently connected to
// the gateway, since log messages aren't tied to any one in-flight request.
func (r *NotificationRouter) HandleLog(ctx context.Context, req *mcp.LoggingMessageRequest) {
	r.mu.Lock()
	server := r.server
	r.mu.Unlock()
	if server == nil {
		return
	}

	for session := range server.Sessions() {
		if err := session.Log(ctx, req.Params); err != nil {
			log.Printf("proxy: forwarding notifications/message to client: %v", err)
		}
	}
}
