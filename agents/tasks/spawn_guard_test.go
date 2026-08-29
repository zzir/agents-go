package tasks

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// flakyStore fails ByChildSession, which is how Spawn learns whether its parent
// is itself a task and therefore how deep the new one would be.
type flakyStore struct {
	Store
	err error
}

func (s flakyStore) ByChildSession(context.Context, string) (*Task, error) { return nil, s.err }

// A store that cannot answer "is this session a task?" must not be read as
// "no". The two answers set different depths, so treating a failed lookup as an
// ordinary parent restarts the count at 1 and one transient query error is
// enough to spawn past MaxDepth.
func TestSpawnRefusesWhenTheDepthLookupFails(t *testing.T) {
	boom := errors.New("database is down")
	h := newHarness(t, func(cfg *Config) {
		cfg.Store = flakyStore{Store: cfg.Store, err: boom}
	})

	_, err := h.m.Spawn(context.Background(), SpawnRequest{
		ParentSessionID: "parent", AgentName: "worker", Input: "do it",
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Spawn err = %v, want it to wrap %v", err, boom)
	}
}

// MetaFor is a host's own depth gate — it decides whether a run gets the task
// tools at all — so it has to report a failed lookup rather than answering
// "not a task", which is the permissive one.
func TestMetaForReportsAStoreFailure(t *testing.T) {
	boom := errors.New("database is down")
	h := newHarness(t, func(cfg *Config) {
		cfg.Store = flakyStore{Store: cfg.Store, err: boom}
	})

	meta, ok, err := h.m.MetaFor(context.Background(), "some-session")
	if !errors.Is(err, boom) {
		t.Fatalf("MetaFor err = %v, want it to wrap %v", err, boom)
	}
	if ok || meta != nil {
		t.Fatalf("MetaFor reported a task despite failing: ok=%v meta=%+v", ok, meta)
	}
}

// The per-parent spawn lock is keyed on a session id, so a table that only
// grows is a leak in a long-lived server. Every path out of Spawn must release
// its entry — including the ones that never create a task.
func TestSpawnLockTableDoesNotGrow(t *testing.T) {
	h := newHarness(t, func(cfg *Config) { cfg.MaxConcurrentPerParent = func() int { return 1 } })
	ctx := context.Background()

	// A successful spawn, an over-cap refusal, and a resolver failure: three
	// different exits, all of which took the lock.
	for i, parent := range []string{"p1", "p2", "p3", "p4", "p5"} {
		for range 3 {
			_, _ = h.m.Spawn(ctx, SpawnRequest{
				ParentSessionID: parent, AgentName: "worker", Input: "do it",
			})
		}
		if got := h.m.spawnLockCount(); got != 0 {
			t.Fatalf("after parent %d (%s) the lock table holds %d entries, want 0", i, parent, got)
		}
	}
}

// Concurrent spawns for one parent share a single entry, and the last one out
// removes it — the point of the reference count.
func TestSpawnLockTableSurvivesConcurrentSpawns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			_, _ = h.m.Spawn(ctx, SpawnRequest{
				ParentSessionID: "parent", AgentName: "worker", Input: "do it",
			})
		})
	}
	wg.Wait()

	if got := h.m.spawnLockCount(); got != 0 {
		t.Fatalf("lock table holds %d entries after every spawn returned, want 0", got)
	}
}
