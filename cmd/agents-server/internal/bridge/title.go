package bridge

import (
	"context"
	"strings"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
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
	log := logging.Ctx(ctx)

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
	var title string
	if err != nil {
		log.Warn("title gen: run failed, using first message", "error", err)
	} else {
		title = strings.Trim(strings.TrimSpace(res.FinalOutputString()), "\"'")
		if len([]rune(title)) > 50 {
			log.Warn("title gen: too long, using first message", "raw", title)
			title = ""
		}
	}
	// A reachable provider that still failed (or garbled the title) leaves the
	// session nameless; fall back to the user's first message so it is not stuck
	// as "New Session". Provider absence bailed earlier — that stays a no-op.
	if title == "" {
		title = fallbackTitle(userInput)
	}
	if title == "" {
		return
	}

	// A CAS on the default name: the model took its time, and a person may
	// have named the session meanwhile — their name stands, and so does the
	// first of two generators'. The run's own bindSessionAgent only ever sets
	// agent_config_id, never the name, so it is no contender.
	won, err := r.Deps.Sessions.NameIfDefault(ctx, sessionID, title)
	if err != nil {
		log.Warn("title gen: save failed", "error", err)
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

// fallbackTitle derives a readable name from the user's first message when the
// title model is unavailable: its first line, trimmed to 50 runes with an
// ellipsis when clipped.
func fallbackTitle(userInput string) string {
	line := userInput
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > 50 {
		return strings.TrimSpace(string(r[:50])) + "…"
	}
	return line
}
