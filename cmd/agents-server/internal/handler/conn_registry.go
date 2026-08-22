package handler

import (
	"context"
	"maps"
	"sync"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ConnRegistry tracks every authenticated WebSocket connection so run events
// behave as a broadcast bus PER OWNER: a run started from any client (this
// connection, another browser, REST) reaches every connection of the
// session's owner, a connection joining mid-run is attached to each of its
// user's in-flight streams with a full replay, and nobody else's connection
// hears a thing (README invariant 42).
type ConnRegistry struct {
	hub      *bridge.RunHub
	sessions *store.SessionStore

	mu    sync.Mutex
	conns map[*server.WSConn]*connSubs
}

// NewConnRegistry returns a registry that attaches connections to runs on hub;
// sessions resolves a broadcast's session to the owner it may reach.
func NewConnRegistry(hub *bridge.RunHub, sessions *store.SessionStore) *ConnRegistry {
	return &ConnRegistry{hub: hub, sessions: sessions, conns: make(map[*server.WSConn]*connSubs)}
}

// register adds a connection and immediately attaches it to every live run of
// its user (replaying each run's buffer from the start), so a browser opened
// mid-run rebuilds the in-flight turn it never saw begin.
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

// AttachAll subscribes every connection of the run's owner to runID with a
// full replay, skipping connections already attached (an approval resume
// keeps the run id — re-subscribing an attached watcher would double-deliver
// every event). Wired to Runner.OnRunAttach, so WS- and REST-created runs and
// approval resumes all broadcast the same way. A record without an owner
// attaches nobody.
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

// Broadcast writes env — a fact about sessionID — to every connection of the
// session's owner not attached to exceptRunID's stream (which already carried
// it): the bus for a fact a run stream cannot reach everyone with
// (Runner.OnBroadcast). No replay: a connection that joins later reads the
// durable rows instead. A session that cannot be resolved reaches nobody.
func (r *ConnRegistry) Broadcast(env *protocol.Envelope, exceptRunID, sessionID string) {
	sess, err := r.sessions.Get(context.Background(), sessionID)
	if err != nil {
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
