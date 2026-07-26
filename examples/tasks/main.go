// Command tasks demonstrates background sub-agents: a tool call spawns a child
// run with its own session, the parent does not wait, and the parent is woken
// with the result when the child finishes.
//
// The interesting part is what the Manager does that a plain "go run an agent"
// does not: the wake-up is owed durably, so a parent that is busy when the task
// finishes is woken at its next boundary rather than never; a cancellation does
// not wake at all; and a task cannot spawn tasks.
//
// Everything environment-specific arrives through three injection points, and
// this program supplies the simplest possible versions of them: agents are a
// map, launching is a goroutine, and waking is allowed whenever the parent is
// not already running.
//
// Run with: OPENAI_API_KEY=... go run ./examples/tasks
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/models/openai"
)

// inherit is this program's opaque configuration payload: which agent a run
// should use. The SDK carries it without looking inside.
type inherit struct {
	Agent string `json:"agent"`
}

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	repo := agents.NewInMemoryRepo()
	store := tasks.NewInMemoryStore()

	// The agents this program can run as. A real host would look these up.
	catalog := map[string]*agents.Agent{
		"researcher": {
			Name:         "researcher",
			Model:        "gpt-4.1-mini",
			Instructions: agents.StaticInstructions("Answer the question in one short sentence."),
		},
	}

	var (
		mu      sync.Mutex
		running = map[string]bool{} // session id → a run is in flight
		wg      sync.WaitGroup
	)
	var mgr *tasks.Manager

	mgr = tasks.New(tasks.Config{
		Store:    store,
		Sessions: repo,

		// "What is this agent called?"
		Resolver: tasks.AgentResolverFunc(func(_ context.Context, _, name string) (tasks.Spec, error) {
			name = cmp.Or(name, "researcher")
			if _, ok := catalog[name]; !ok {
				return tasks.Spec{}, fmt.Errorf("no agent named %q", name)
			}
			raw, _ := json.Marshal(inherit{Agent: name})
			return tasks.Spec{DisplayName: name, Inherit: raw}, nil
		}),

		// "Start a run." It returns immediately; the run happens on its own
		// goroutine and reports back through OnRunFinished.
		Launcher: tasks.LauncherFunc(func(_ context.Context, req tasks.LaunchRequest) error {
			mu.Lock()
			if running[req.SessionID] {
				mu.Unlock()
				// Losing this race is normal and not an error: the debt stays
				// pending and the winner's boundary re-drains it.
				return fmt.Errorf("session %s is busy", req.SessionID)
			}
			running[req.SessionID] = true
			mu.Unlock()

			var in inherit
			_ = json.Unmarshal(req.Inherit, &in)
			agent := catalog[in.Agent]
			if agent == nil {
				agent = catalog["researcher"]
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(running, req.SessionID)
					mu.Unlock()
				}()

				if req.Wake {
					fmt.Printf("\n  ↩ parent woken:\n     %s\n", req.Input)
				}
				sess, err := repo.Open(context.Background(), req.SessionID)
				if err != nil {
					log.Println("open session:", err)
					return
				}
				out := tasks.RunOutcome{Status: tasks.StatusCompleted}
				res, err := agents.RunSync(context.Background(), agent, req.Input, agents.RunOptions{
					Model:        agents.ModelOptions{Provider: provider},
					Conversation: agents.ConversationOptions{Session: sess},
				})
				if err != nil {
					out.Status, out.Err = tasks.StatusFailed, err.Error()
				} else {
					out.Text = res.FinalOutputString()
				}
				// The single entry point that advances task state — for task
				// sessions AND parent sessions.
				mgr.OnRunFinished(context.Background(), req.SessionID, out)
			}()
			return nil
		}),

		// "May this parent be woken right now?" A real host adds "not being
		// deleted" and "not paused on an approval".
		Guard: tasks.WakeGuardFunc(func(_ context.Context, sessionID string) bool {
			mu.Lock()
			defer mu.Unlock()
			return !running[sessionID]
		}),

		OnTaskUpdate: func(_ context.Context, t *tasks.Task) {
			fmt.Printf("  • task %q → %s\n", t.Label, t.Status)
		},
	})

	// The parent conversation.
	parent, err := repo.Create(ctx, agents.CreateOptions{ID: "parent", Title: "chat"})
	if err != nil {
		log.Fatal(err)
	}
	_ = parent

	coordinator := &agents.Agent{
		Name:  "coordinator",
		Model: "gpt-4.1-mini",
		Instructions: agents.StaticInstructions(
			"Delegate research to background tasks with spawn_task, then finish your turn. " +
				"Do not poll — you will be notified when a task finishes."),
		Tools: mgr.Tools(nil),
	}

	fmt.Println("parent turn — the coordinator may delegate…")
	res, err := agents.RunSync(ctx, coordinator, "Find out when the transistor was invented.", agents.RunOptions{
		Model: agents.ModelOptions{Provider: provider},
		// The tools read the parent session id from here.
		Context: "parent",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("parent said: %s\n", res.FinalOutputString())

	// Spawn one directly as well, so the lifecycle below runs whatever the
	// model decided to do. This is the same call spawn_task makes.
	fmt.Println("\nspawning one directly…")
	if _, err := mgr.Spawn(ctx, tasks.SpawnRequest{
		ParentSessionID: "parent",
		AgentName:       "researcher",
		Input:           "When was the transistor invented?",
		Label:           "transistor date",
	}); err != nil {
		log.Fatal(err)
	}

	// Let the task finish and the wake-up land. A server would not wait like
	// this — its own run boundaries drive everything.
	wg.Wait()
	time.Sleep(100 * time.Millisecond)
	wg.Wait()

	// Nothing is owed any more: the wake-up was delivered.
	if owed, err := store.PendingNotifyParents(ctx); err == nil && len(owed) > 0 {
		fmt.Printf("\nstill owed a wake-up: %v\n", owed)
	}

	all, err := store.ListByParent(ctx, "parent")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n%d task(s):\n", len(all))
	for _, t := range all {
		fmt.Printf("  %s  %-10s notify=%-9s %s\n", t.ID[:8], t.Status, t.NotifyState, t.Summary)
	}
}
