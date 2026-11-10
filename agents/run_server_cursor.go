package agents

// serverCursor tracks what a server-managed conversation already holds, so
// each turn sends only the delta; the zero value resends the full input.
// RunState serializes it as is.
type serverCursor struct {
	ResponseID         string `json:"response_id,omitempty"`
	ItemCount          int    `json:"item_count,omitempty"`
	ConversationActive bool   `json:"conversation_active,omitempty"`
}

// buildTurnInput assembles one turn's model input: the delta past the cursor
// under server-managed state, else the full history (usedOriginal reports it).
func (r *runner) buildTurnInput(cur serverCursor, originalInput []InputItem, generated []*RunItem) (in []InputItem, prevID string, usedOriginal bool, err error) {
	switch {
	case r.opts.Conversation.UsePreviousResponseID && cur.ResponseID != "":
		in, err = itemsToInputList(generated[cur.ItemCount:])
		prevID = cur.ResponseID
	case r.opts.Conversation.ConversationID != "" && cur.ConversationActive:
		// The conversation already holds prior items server-side.
		in, err = itemsToInputList(generated[cur.ItemCount:])
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

// advance moves the cursor past a completed turn; synthesized items stay past
// it. Not called for a resumed turn, whose pause-time cursor already accounts for it.
func (cur *serverCursor) advance(opts ConversationOptions, resp *ModelResponse, lenBeforeStep, modelItems int) {
	served := lenBeforeStep + modelItems
	if opts.UsePreviousResponseID && resp.ResponseID != "" {
		cur.ResponseID = resp.ResponseID
		cur.ItemCount = served
	}
	if opts.ConversationID != "" {
		cur.ConversationActive = true
		cur.ItemCount = served
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
