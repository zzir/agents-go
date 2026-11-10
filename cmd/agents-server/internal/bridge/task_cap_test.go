package bridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	sdktasks "github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The per-parent cap has one gate, the task manager's: concurrent spawns in one
// turn are counted and created under the parent's spawn lock, so exactly the
// cap's worth start and every other is refused — decisions §5.118.
func TestConcurrentSpawnsStopAtTheCap(t *testing.T) {
	ctx := context.Background()
	// A model that never answers keeps every started task live.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	runner, sessions, tasks, agentConfigs := newTaskTestRunner(t)
	fakeModelAgent(t, runner.db, agentConfigs, srv.URL)
	const limit, attempts = 2, 8
	if err := store.NewSettingStore(runner.db).Set(ctx, settings.KeyMaxTasksPerSession, "2"); err != nil {
		t.Fatal(err)
	}
	parent := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		started []string
		refused int
	)
	for range attempts {
		wg.Go(func() {
			info, err := runner.tasks.Spawn(ctx, sdktasks.SpawnRequest{ParentSessionID: parent.ID, AgentName: "worker", Input: "go"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				started = append(started, info.TaskID)
			case errors.Is(err, sdktasks.ErrTaskLimit):
				refused++
			default:
				t.Errorf("spawn: %v", err)
			}
		})
	}
	wg.Wait()
	if len(started) != limit || refused != attempts-limit {
		t.Fatalf("started %d, refused %d of %d concurrent spawns; want %d and %d", len(started), refused, attempts, limit, attempts-limit)
	}
	for _, id := range started {
		if _, err := runner.StopTask(id, false); err != nil {
			t.Fatalf("StopTask: %v", err)
		}
		if got := awaitTaskStatus(t, tasks, id, 5*time.Second); got != "cancelled" {
			t.Fatalf("row = %q after the stop, want cancelled", got)
		}
	}
}
