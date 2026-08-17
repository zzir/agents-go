package bridge

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A new session is named from its first prompt, by a throwaway one-turn agent
// running beside the real run.

// maybeGenerateTitle names a still-default ("New Session") session from the user's
// first message. It runs IN PARALLEL with the run — the title depends only on
// the user's message, not the answer — so it is fired at run start and takes the
// input, model and provider directly rather than reading them back after the run
// (at run start the SDK has not persisted anything yet). It runs on the hub root
// context so it survives the client disconnecting.
func (r *Runner) maybeGenerateTitle(parentCtx context.Context, sessionID, model, userInput string, provider agents.ModelProvider, sendEvent func(string, any)) {
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()
	log := zerolog.Ctx(ctx)

	// Only name an unnamed session. Checked first so a re-run on an already-named
	// session (every message after the first) is a cheap Get + return.
	sess, err := r.Deps.Sessions.Get(ctx, sessionID)
	if err != nil || sess.Name != store.DefaultSessionName {
		return
	}
	if userInput == "" || provider == nil {
		return
	}

	titleAgent := &agents.Agent{
		Name:         "title_gen",
		Model:        model,
		Instructions: agents.StaticInstructions("You generate concise chat titles. Reply with ONLY the title text, nothing else. No quotes. Under 30 characters."),
	}
	prompt := "Generate a short title for this chat:\n\n" + userInput
	res, err := agents.RunSync(ctx, titleAgent, prompt, agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 1}, Model: agents.ModelOptions{Provider: provider}})
	if err != nil {
		log.Warn().Err(err).Msg("title gen: run failed")
		return
	}
	title := strings.TrimSpace(res.FinalOutputString())
	title = strings.Trim(title, "\"'")
	if title == "" || len([]rune(title)) > 50 {
		log.Warn().Str("raw", title).Msg("title gen: empty or too long")
		return
	}

	// A CAS on the default name: the model took its time, and a person may
	// have named the session meanwhile — their name stands, and so does the
	// first of two generators'. The run's own bindSessionAgent only ever sets
	// agent_config_id, never the name, so it is no contender.
	won, err := r.Deps.Sessions.NameIfDefault(ctx, sessionID, title)
	if err != nil {
		log.Warn().Err(err).Msg("title gen: save failed")
		return
	}
	if !won {
		return
	}
	sendEvent(protocol.EventSessionTitleUpdated, protocol.SessionTitleUpdated{
		SessionID: sessionID,
		Title:     title,
	})
}
