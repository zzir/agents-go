package agents

// serverCursor tracks what a server-managed conversation already holds, so
// each turn sends only the delta. The zero value means locally-managed
// history: every turn resends the full input.
//
// Two server modes share it. In previous_response_id mode responseID chains
// the calls; in conversation-id mode conversationActive marks that the server
// holds prior items (from the second turn on). itemCount marks how many
// generated items the server already has; items past it — tool outputs
// synthesized locally after the model call — are the next turn's delta.
type serverCursor struct {
	responseID         string
	itemCount          int
	conversationActive bool
}

// buildTurnInput assembles one turn's model input. In previous_response_id
// mode, only the items the server does not yet have are sent and prevID
// chains the rest; likewise in conversation-id mode once the server holds
// history. Otherwise the full history is rebuilt — original input plus every
// generated item — and usedOriginal reports it, because only input that is
// sent in full can be rewritten by an input guardrail's Replace verdict.
//
// It runs again within the first turn when a Blocking input guardrail
// REPLACES the input: the guarded call itself must see the rewritten input.
func (r *runner) buildTurnInput(cur serverCursor, originalInput []InputItem, generated []*RunItem) (in []InputItem, prevID string, usedOriginal bool, err error) {
	switch {
	case r.opts.Conversation.UsePreviousResponseID && cur.responseID != "":
		in, err = itemsToInputList(generated[cur.itemCount:])
		prevID = cur.responseID
	case r.opts.Conversation.ConversationID != "" && cur.conversationActive:
		// The conversation already holds prior items server-side.
		in, err = itemsToInputList(generated[cur.itemCount:])
	default:
		usedOriginal = true
		in, err = buildModelInput(originalInput, generated)
	}
	if err != nil {
		return nil, "", false, err
	}
	// Optionally strip reasoning-item ids before sending them to the model.
	return applyReasoningItemIDPolicy(in, r.opts.Exec.ReasoningItemIDPolicy), prevID, usedOriginal, nil
}

// advance moves the cursor past a completed turn: the server now has
// everything sent this turn plus the model's own output items, while
// synthesized items (tool outputs) stay past the cursor, pending for the next
// call. A no-op for locally-managed history.
//
// It is NOT called for a resumed turn: that turn re-processes the interrupted
// response, which the pause-time cursor (restored from the RunState) already
// accounts for. Advancing there would mark the tool outputs completed before
// the pause as already-served, and the server would never receive them.
func (cur *serverCursor) advance(opts ConversationOptions, resp *ModelResponse, lenBeforeStep, modelItems int) {
	served := lenBeforeStep + modelItems
	if opts.UsePreviousResponseID && resp.ResponseID != "" {
		cur.responseID = resp.ResponseID
		cur.itemCount = served
	}
	if opts.ConversationID != "" {
		cur.conversationActive = true
		cur.itemCount = served
	}
}

// buildModelInput assembles the model input for a turn: the original input
// followed by every generated item converted back to input form.
func buildModelInput(originalInput []InputItem, generated []*RunItem) ([]InputItem, error) {
	genInput, err := itemsToInputList(generated)
	if err != nil {
		return nil, err
	}
	out := make([]InputItem, 0, len(originalInput)+len(genInput))
	out = append(out, originalInput...)
	out = append(out, genInput...)
	return out, nil
}
