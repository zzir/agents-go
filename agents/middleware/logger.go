package middleware

import (
	"context"
	"log/slog"
	"time"

	"github.com/zzir/agents-go/agents"
)

// Logging logs a run's shape: when it starts, what it produced, how it ended.
//
// It exists as much to demonstrate the interface as to be used — an observing
// middleware calls next, ranges the stream it gets back, and re-yields
// everything unchanged. Note what it must not do: swallow an event, or return
// before the consumer has seen the terminal one.
type Logging struct {
	// Logger receives the records. Nil uses slog.Default.
	Logger *slog.Logger
	// LogItems logs each run item as it is produced. Off by default: on a long
	// run that is a lot of output, and the interesting records are the ends.
	LogItems bool
}

// Run implements agents.RunMiddleware.
func (m Logging) Run(ctx context.Context, next agents.RunFunc, in agents.RunInput) agents.RunStream {
	log := m.Logger
	if log == nil {
		log = slog.Default()
	}
	agentName := ""
	if in.Agent != nil {
		agentName = in.Agent.Name
	}

	return func(yield func(agents.StreamEvent, error) bool) {
		start := time.Now()
		log.InfoContext(ctx, "run started", "agent", agentName, "input_items", len(in.Input))

		items := 0
		for ev, err := range next(ctx, in) {
			if err != nil {
				log.ErrorContext(ctx, "run failed",
					"agent", agentName,
					"error", err,
					"code", agents.CodeOf(err),
					"duration", time.Since(start))
				yield(nil, err)
				return
			}
			switch e := ev.(type) {
			case *agents.RunItemStreamEvent:
				items++
				if m.LogItems {
					log.DebugContext(ctx, "run item", "agent", agentName, "item", e.Name)
				}
			case *agents.RunCompletedEvent:
				log.InfoContext(ctx, "run finished",
					"agent", agentName,
					"items", items,
					"turns", turnsOf(e.Result),
					"duration", time.Since(start))
			}
			if !yield(ev, nil) {
				// The consumer stopped. Say so — an abandoned run looks like a
				// hang in a log that only records completions.
				log.InfoContext(ctx, "run abandoned by consumer",
					"agent", agentName, "items", items, "duration", time.Since(start))
				return
			}
		}
	}
}

func turnsOf(res *agents.RunResult) int {
	if res == nil {
		return 0
	}
	return len(res.RawResponses)
}
