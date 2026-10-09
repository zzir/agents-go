package handler

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ConnRegistry tracks every authenticated WebSocket connection so run events
// behave as a broadcast bus per owner — invariant 14.
type ConnRegistry struct {
	hub      *bridge.RunHub
	sessions *store.SessionStore

	mu    sync.Mutex
	conns map[*server.WSConn]*connSubs
}

// broadcastResolveTimeout bounds a broadcast's owner lookup.
const broadcastResolveTimeout = 10 * time.Second

// NewConnRegistry returns a registry that attaches connections to runs on hub;
// sessions resolves a broadcast's session to the owner it may reach.
func NewConnRegistry(hub *bridge.RunHub, sessions *store.SessionStore) *ConnRegistry {
	return &ConnRegistry{hub: hub, sessions: sessions, conns: make(map[*server.WSConn]*connSubs)}
}

// register adds a connection and attaches it to every live run of its user
// with a full replay.
func (r *ConnRegistry) register(conn *server.WSConn, subs *connSubs) {
	r.mu.Lock()
	r.conns[conn] = subs
	r.mu.Unlock()
	for _, runID := range r.hub.LiveRunIDs() {
		if info, ok := r.hub.Info(runID); ok && info.OwnerID == conn.User.ID {
			r.attach(conn, subs, runID)
		}
	}
}

// unregister drops a closed connection. Its hub subscriptions are detached by
// the caller (connSubs.closeAll).
func (r *ConnRegistry) unregister(conn *server.WSConn) {
	r.mu.Lock()
	delete(r.conns, conn)
	r.mu.Unlock()
}

// AttachAll subscribes every connection of the run's owner to runID with a full
// replay, skipping those already attached (Runner.OnRunAttach); no owner, nobody.
func (r *ConnRegistry) AttachAll(runID string) {
	info, ok := r.hub.Info(runID)
	if !ok || info.OwnerID == "" {
		return
	}
	r.mu.Lock()
	pairs := make(map[*server.WSConn]*connSubs, len(r.conns))
	maps.Copy(pairs, r.conns)
	r.mu.Unlock()
	for conn, subs := range pairs {
		if conn.User.ID == info.OwnerID && !subs.has(runID) {
			r.attach(conn, subs, runID)
		}
	}
}

// Broadcast writes env to every connection of sessionID's owner not attached to
// exceptRunID's stream (Runner.OnBroadcast); no replay, and an unresolved
// session reaches nobody.
func (r *ConnRegistry) Broadcast(ctx context.Context, env *protocol.Envelope, exceptRunID, sessionID string) {
	// Detached: the caller's ctx is often a run's that just ended.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), broadcastResolveTimeout)
	defer cancel()
	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		logging.Ctx(ctx).Warn("broadcast: resolving session", "error", err, "session_id", sessionID)
		return
	}
	r.mu.Lock()
	conns := make([]*server.WSConn, 0, len(r.conns))
	for conn, subs := range r.conns {
		if conn.User.ID != sess.OwnerID || (exceptRunID != "" && subs.has(exceptRunID)) {
			continue
		}
		conns = append(conns, conn)
	}
	r.mu.Unlock()
	for _, conn := range conns {
		wsSink(conn)(env)
	}
}

// attach subscribes one connection to runID from seq 0; a run already gone
// from the hub (finished + GC'd between listing and attaching) is skipped.
func (r *ConnRegistry) attach(conn *server.WSConn, subs *connSubs, runID string) {
	if cancel, ok := r.hub.Subscribe(runID, 0, wsSink(conn)); ok {
		subs.add(runID, cancel)
	}
}
