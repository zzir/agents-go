// Command tasks demonstrates background sub-agents: a tool call spawns a child
// run with its own session, the parent does not wait, and it is woken with the
// result at its next boundary. The host supplies agents, launching and waking
// through three injection points — see spec §2.13 and docs/howto/tasks.md.
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
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/models/openai"
)

// inherit is the host's opaque configuration payload: which agent a run uses.
type inherit struct {
	Agent string `json:"agent"`
}

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	repo := session.NewInMemoryRepo()
	store := tasks.NewInMemoryStore()

	// The agents this program can run as. A real host would look these up.
	researcher := &agents.Agent{
		Name:         "researcher",
		Model:        "gpt-4.1-mini",
		Instructions: agents.StaticInstructions("Answer the question in one short sentence."),
	}
	catalog := map[string]*agents.Agent{"researcher": researcher}

	var (
		mu      sync.Mutex
		running = map[string]bool{} // session id → a run is in flight
		wg      sync.WaitGroup
	)
	var mgr *tasks.Manager

	mgr = tasks.New(tasks.Config{
		Store:    store,
		Sessions: repo,

		// name is "" when the coordinator spawns itself; only the researcher
		// runs tasks here.
		Resolver: tasks.AgentResolver(func(_ context.Context, _, name string) (tasks.Spec, error) {
			name = cmp.Or(name, "researcher")
			if _, ok := catalog[name]; !ok {
				return tasks.Spec{}, fmt.Errorf("no agent named %q", name)
			}
			raw, _ := json.Marshal(inherit{Agent: name})
			return tasks.Spec{DisplayName: name, Inherit: raw}, nil
		}),

		// Starts a run on its own goroutine; it reports back through OnRunFinished.
		Launcher: tasks.Launcher(func(_ context.Context, req tasks.LaunchRequest) error {
			mu.Lock()
			if running[req.SessionID] {
				mu.Unlock()
				// Losing this race is not an error: the debt stays pending for
				// the winner's boundary.
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

			wg.Go(func() {
				defer func() {
					mu.Lock()
					delete(running, req.SessionID)
					mu.Unlock()
				}()

				sess, err := repo.Open(context.Background(), req.SessionID)
				if err != nil {
					log.Println("open session:", err)
					return
				}
				// RunID names the attempt, so a retry's new attempt is not
				// overwritten — see docs/howto/tasks.md.
				out := tasks.RunOutcome{RunID: req.RunID, Status: tasks.StatusCompleted}
				res, err := agents.RunSync(context.Background(), agent, req.Input, agents.RunOptions{
					Model:        agents.ModelOptions{Provider: provider},
					Conversation: agents.ConversationOptions{Session: sess},
				})
				if err != nil {
					out.Status, out.Err = tasks.StatusFailed, err.Error()
				} else {
					out.Text = res.FinalOutputString()
				}
				// The single entry point that advances task state, for task and
				// parent sessions.
				mgr.OnRunFinished(context.Background(), req.SessionID, out)
			})
			return nil
		}),

		// A task ended and its parent has not heard; a real host records a
		// durable debt here.
		OnFinished: func(_ context.Context, t *tasks.Task) {
			fmt.Printf("  • %q finished (%s) — its parent owes itself a turn\n", t.Label, t.Status)
		},

		OnTaskUpdate: func(_ context.Context, t *tasks.Task) {
			fmt.Printf("  • task %q → %s\n", t.Label, t.Status)
		},
	})

	// The parent conversation.
	parent, err := repo.Create(ctx, session.CreateOptions{ID: "parent", Title: "chat"})
	if err != nil {
		log.Fatal(err)
	}
	_ = parent

	// spawn_task may name the coordinator's handoff targets: the researcher.
	coordinator := &agents.Agent{
		Name:  "coordinator",
		Model: "gpt-4.1-mini",
		Instructions: agents.StaticInstructions(
			"Delegate research to the researcher as a background task with spawn_task, then finish your turn. " +
				"Do not poll — you will be notified when a task finishes."),
		Handoffs: []agents.Handoff{agents.HandoffTo(researcher)},
		Tools:    mgr.Tools(nil),
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

	// Spawn one directly too (the same call spawn_task makes), whatever the
	// model decided.
	fmt.Println("\nspawning one directly…")
	if _, err := mgr.Spawn(ctx, tasks.SpawnRequest{
		ParentSessionID: "parent",
		AgentName:       "researcher",
		Input:           "When was the transistor invented?",
		Label:           "transistor date",
	}); err != nil {
		log.Fatal(err)
	}

	// Let the task finish and the wake-up land; a server's own run boundaries
	// drive this.
	wg.Wait()
	time.Sleep(100 * time.Millisecond)
	wg.Wait()

	all, err := store.ListByParent(ctx, "parent")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n%d task(s):\n", len(all))
	for _, t := range all {
		fmt.Printf("  %s  %-10s %s\n", t.ID[:8], t.Status, t.Summary)
	}
}
