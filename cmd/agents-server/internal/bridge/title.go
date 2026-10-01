package bridge

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A new session is named from its first prompt, by a throwaway one-turn agent
// running beside the real run.

// maybeGenerateTitle names a still-default session from the user's first
// message, IN PARALLEL with the run, on the hub root context.
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
		title = store.ClipName(plainTitle(res.FinalOutputString()))
	}
	// A reachable provider that failed or garbled the title leaves the session
	// nameless; fall back to the first message.
	if title == "" {
		title = fallbackTitle(userInput)
	}
	if title == "" {
		return
	}

	// A CAS on the default name: a person may have named the session
	// meanwhile — their name stands, and so does the first of two generators'.
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

// fallbackTitle derives a name from the user's first message: its first line,
// clipped like every machine-made name.
func fallbackTitle(userInput string) string {
	line := userInput
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	return store.ClipName(plainTitle(line))
}

var (
	// A heading, quote or list marker that opens the title.
	titleLeadRE = regexp.MustCompile(`^(#{1,6}|>|[-*+]|\d+\.) `)
	// A link that is the whole title.
	titleLinkRE = regexp.MustCompile(`^\[([^\[\]]+)\]\([^()\s]*\)$`)
	// The marks a title may be wrapped in, the longer of a kind first.
	titleWraps = [][2]string{{"**", "**"}, {"__", "__"}, {"*", "*"}, {"_", "_"}, {"`", "`"}, {`"`, `"`}, {"'", "'"}, {"“", "”"}, {"‘", "’"}}
)

// plainTitle strips the markdown that wraps a WHOLE title — the sidebar shows a
// name as text — and collapses its whitespace. Marks inside it stay: the title
// may be the person's own words (user_id, *.go, 2 * 3).
func plainTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for {
		t := unwrapTitle(s)
		if t == s {
			return s
		}
		s = t
	}
}

// unwrapTitle takes one layer off: a leading marker, a link around everything,
// or a pair of marks whose inside holds none of them.
func unwrapTitle(s string) string {
	if loc := titleLeadRE.FindStringIndex(s); loc != nil {
		return strings.TrimSpace(s[loc[1]:])
	}
	if m := titleLinkRE.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	for _, w := range titleWraps {
		if len(s) < len(w[0])+len(w[1]) || !strings.HasPrefix(s, w[0]) || !strings.HasSuffix(s, w[1]) {
			continue
		}
		inner := s[len(w[0]) : len(s)-len(w[1])]
		if !strings.Contains(inner, w[0]) && !strings.Contains(inner, w[1]) {
			return strings.TrimSpace(inner)
		}
	}
	return s
}
