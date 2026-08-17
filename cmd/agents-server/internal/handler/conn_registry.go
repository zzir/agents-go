package handler

import (
	"maps"
	"sync"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
)

// ConnRegistry tracks every authenticated WebSocket connection so run events
// behave as a broadcast bus: a run started from ANY client (this connection,
// another browser, REST) is attached to ALL connections, and a connection that
// joins mid-run is attached to every in-flight stream with a full replay.
// Without it, run events were a private reply channel to whichever connection
// happened to start the run — a second browser on the same session saw nothing
// until a manual reload.
type ConnRegistry struct {
	hub *bridge.RunHub

	mu    sync.Mutex
	conns map[*server.WSConn]*connSubs
}

// NewConnRegistry returns a registry that attaches connections to runs on hub.
func NewConnRegistry(hub *bridge.RunHub) *ConnRegistry {
	return &ConnRegistry{hub: hub, conns: make(map[*server.WSConn]*connSubs)}
}

// register adds a connection and immediately attaches it to every live run
// (replaying each run's buffer from the start), so a browser opened mid-run
// rebuilds the in-flight turn it never saw begin.
func (r *ConnRegistry) register(conn *server.WSConn, subs *connSubs) {
	r.mu.Lock()
	r.conns[conn] = subs
	r.mu.Unlock()
	for _, runID := range r.hub.LiveRunIDs() {
		r.attach(conn, subs, runID)
	}
}

// unregister drops a closed connection. Its hub subscriptions are detached by
// the caller (connSubs.closeAll).
func (r *ConnRegistry) unregister(conn *server.WSConn) {
	r.mu.Lock()
	delete(r.conns, conn)
	r.mu.Unlock()
}

// AttachAll subscribes every registered connection to runID with a full
// replay, skipping connections already attached (an approval resume keeps the
// run id — re-subscribing an attached watcher would double-deliver every
// event). Wired to Runner.OnRunAttach, so WS- and REST-created runs and
// approval resumes all broadcast the same way.
func (r *ConnRegistry) AttachAll(runID string) {
	r.mu.Lock()
	pairs := make(map[*server.WSConn]*connSubs, len(r.conns))
	maps.Copy(pairs, r.conns)
	r.mu.Unlock()
	for conn, subs := range pairs {
		if !subs.has(runID) {
			r.attach(conn, subs, runID)
		}
	}
}

// Broadcast writes env to every registered connection not attached to
// exceptRunID's stream (which already carried it) — the bus for a fact a run
// stream cannot reach everyone with (Runner.OnBroadcast). No replay: a
// connection that joins later reads the durable rows instead.
func (r *ConnRegistry) Broadcast(env *protocol.Envelope, exceptRunID string) {
	r.mu.Lock()
	conns := make([]*server.WSConn, 0, len(r.conns))
	for conn, subs := range r.conns {
		if exceptRunID != "" && subs.has(exceptRunID) {
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
