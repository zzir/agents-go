package modelkit

import (
	"encoding/json"
	"fmt"

	"github.com/zzir/agents-go/agents"
)

// OutputItemFromJSON decodes canonical wire JSON into an output item — the only
// way to build one outside openai-go: a union built field-by-field has no
// RawJSON, and such an item cannot become model input or a session entry.
func OutputItemFromJSON(raw []byte) (agents.OutputItem, error) {
	var item agents.OutputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return item, fmt.Errorf("modelkit: decoding output item: %w", err)
	}
	if item.RawJSON() == "" {
		return item, fmt.Errorf("modelkit: output item decoded without raw JSON")
	}
	return item, nil
}

// messagePartJSON is the wire shape of an output_text content part.
type messagePartJSON struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

// MessageItem synthesizes a canonical assistant message item with one
// output_text part per text. Several parts is how a backend that splits one
// assistant turn into consecutive text blocks keeps them ONE message: the
// runner reads only a turn's last message item. id may be empty when the
// backend does not assign item ids.
func MessageItem(id string, texts ...string) (agents.OutputItem, error) {
	parts := make([]messagePartJSON, 0, len(texts))
	for _, text := range texts {
		parts = append(parts, messagePartJSON{Type: "output_text", Text: text, Annotations: []any{}})
	}
	raw, err := json.Marshal(struct {
		ID      string            `json:"id"`
		Type    string            `json:"type"`
		Role    string            `json:"role"`
		Status  string            `json:"status"`
		Content []messagePartJSON `json:"content"`
	}{
		ID:      id,
		Type:    "message",
		Role:    "assistant",
		Status:  "completed",
		Content: parts,
	})
	if err != nil {
		return agents.OutputItem{}, err
	}
	return OutputItemFromJSON(raw)
}

// RefusalItem synthesizes a canonical assistant message whose single part is a
// REFUSAL — the part type is what the runner recognizes as one; a backend that
// reports refusal out-of-band must not hand the text over as output_text.
func RefusalItem(id, refusal string) (agents.OutputItem, error) {
	raw, err := json.Marshal(struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Status  string `json:"status"`
		Content []struct {
			Type    string `json:"type"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	}{
		ID:     id,
		Type:   "message",
		Role:   "assistant",
		Status: "completed",
		Content: []struct {
			Type    string `json:"type"`
			Refusal string `json:"refusal"`
		}{{Type: "refusal", Refusal: refusal}},
	})
	if err != nil {
		return agents.OutputItem{}, err
	}
	return OutputItemFromJSON(raw)
}

// FunctionCallItem synthesizes a canonical function_call item. argumentsJSON is
// the tool arguments as a JSON document; empty means "{}" — the Responses
// format carries arguments as a string that must itself parse as JSON, and the
// runner hands it to the tool's argument decoder verbatim.
func FunctionCallItem(id, callID, name, argumentsJSON string) (agents.OutputItem, error) {
	if argumentsJSON == "" {
		argumentsJSON = "{}"
	}
	raw, err := json.Marshal(struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Status    string `json:"status"`
	}{
		ID:        id,
		Type:      "function_call",
		CallID:    callID,
		Name:      name,
		Arguments: argumentsJSON,
		Status:    "completed",
	})
	if err != nil {
		return agents.OutputItem{}, err
	}
	return OutputItemFromJSON(raw)
}

// reasoningTextJSON is the wire shape of a reasoning content part.
type reasoningTextJSON struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ReasoningItem synthesizes a canonical reasoning item. text goes into the
// content parts (reasoning_text), not the summary, where raw reasoning text
// belongs; encryptedContent is the adapter's opaque continuity blob (thinking
// signature, redacted reasoning, …), kept across session storage. text may be "".
func ReasoningItem(id, text, encryptedContent string) (agents.OutputItem, error) {
	var content []reasoningTextJSON
	if text != "" {
		content = []reasoningTextJSON{{Type: "reasoning_text", Text: text}}
	}
	raw, err := json.Marshal(struct {
		ID               string              `json:"id"`
		Type             string              `json:"type"`
		Summary          []reasoningTextJSON `json:"summary"`
		Content          []reasoningTextJSON `json:"content,omitempty"`
		EncryptedContent string              `json:"encrypted_content,omitempty"`
	}{
		ID:               id,
		Type:             "reasoning",
		Summary:          []reasoningTextJSON{},
		Content:          content,
		EncryptedContent: encryptedContent,
	})
	if err != nil {
		return agents.OutputItem{}, err
	}
	return OutputItemFromJSON(raw)
}
