package modelkit

import (
	"encoding/json"
	"fmt"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

// Item is one canonical input item in the neutral form adapters map from.
// Which fields are meaningful depends on Type:
//
//   - "message": Role and Parts.
//   - "function_call": ID, CallID, Name, Arguments.
//   - "function_call_output": CallID and Output (a string result is normalized
//     to a single input_text part, so adapters handle one shape).
//   - "reasoning": ID, SummaryTexts, ContentTexts, EncryptedContent.
//   - anything else: only Raw; the adapter decides whether to reject, skip or
//     pass it through.
//
// Raw always holds the item's wire JSON.
type Item struct {
	Type             string
	Role             string
	Parts            []Part
	ID               string
	CallID           string
	Name             string
	Arguments        string
	Output           []Part
	SummaryTexts     []string
	ContentTexts     []string
	EncryptedContent string
	Raw              json.RawMessage
}

// Part is one content part of a message or tool result. Type is the wire type;
// text-bearing parts ("input_text", "output_text", "text") set Text, refusal
// parts set Refusal, and image/file parts set their respective fields.
type Part struct {
	Type     string
	Text     string
	Refusal  string
	ImageURL string
	Detail   string
	FileID   string
	FileData string
	FileURL  string
	Filename string
}

// IsText reports whether the part carries plain text in Text.
func (p Part) IsText() bool {
	switch p.Type {
	case "input_text", "output_text", "text":
		return true
	}
	return false
}

// itemProbe is the superset wire shape ParseInput decodes each item into.
type itemProbe struct {
	Type             string          `json:"type"`
	Role             string          `json:"role"`
	ID               string          `json:"id"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Arguments        string          `json:"arguments"`
	Content          json.RawMessage `json:"content"`
	Output           json.RawMessage `json:"output"`
	Summary          []textProbe     `json:"summary"`
	EncryptedContent string          `json:"encrypted_content"`
}

type textProbe struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type partProbe struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Refusal  string `json:"refusal"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail"`
	FileID   string `json:"file_id"`
	FileData string `json:"file_data"`
	FileURL  string `json:"file_url"`
	Filename string `json:"filename"`
}

// ParseInput walks canonical input items into their neutral form. It works on
// the wire JSON rather than the typed union — the one representation every
// item, including one restored from a session by another build, is sure to have.
func ParseInput(items []agents.InputItem) ([]Item, error) {
	out := make([]Item, 0, len(items))
	for i := range items {
		raw, err := session.MarshalInputItem(items[i])
		if err != nil {
			return nil, fmt.Errorf("modelkit: marshaling input item %d: %w", i, err)
		}
		item, err := parseItem(raw)
		if err != nil {
			return nil, fmt.Errorf("modelkit: input item %d: %w", i, err)
		}
		out = append(out, item)
	}
	return out, nil
}

func parseItem(raw json.RawMessage) (Item, error) {
	var p itemProbe
	if err := json.Unmarshal(raw, &p); err != nil {
		return Item{}, err
	}
	typ := p.Type
	if typ == "" && p.Role != "" {
		// An "easy" message serializes without a type discriminator.
		typ = "message"
	}
	item := Item{Type: typ, Role: p.Role, ID: p.ID, Raw: raw}
	switch typ {
	case "message":
		parts, err := parseParts(p.Content)
		if err != nil {
			return Item{}, fmt.Errorf("message content: %w", err)
		}
		item.Parts = parts
	case "function_call":
		item.CallID = p.CallID
		item.Name = p.Name
		item.Arguments = p.Arguments
	case "function_call_output":
		item.CallID = p.CallID
		parts, err := parseParts(p.Output)
		if err != nil {
			return Item{}, fmt.Errorf("function_call_output output: %w", err)
		}
		item.Output = parts
	case "reasoning":
		for _, s := range p.Summary {
			if s.Text != "" {
				item.SummaryTexts = append(item.SummaryTexts, s.Text)
			}
		}
		var content []textProbe
		if len(p.Content) > 0 {
			if err := json.Unmarshal(p.Content, &content); err != nil {
				return Item{}, fmt.Errorf("reasoning content: %w", err)
			}
		}
		for _, c := range content {
			if c.Text != "" {
				item.ContentTexts = append(item.ContentTexts, c.Text)
			}
		}
		item.EncryptedContent = p.EncryptedContent
	}
	return item, nil
}

// parseParts decodes content the Responses format allows as a bare string or a
// part list; a string is normalized to a single input_text part.
func parseParts(raw json.RawMessage) ([]Part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []Part{{Type: "input_text", Text: s}}, nil
	}
	var probes []partProbe
	if err := json.Unmarshal(raw, &probes); err != nil {
		return nil, fmt.Errorf("content is neither a string nor a part list: %w", err)
	}
	parts := make([]Part, len(probes))
	for i, p := range probes {
		parts[i] = Part(p)
	}
	return parts, nil
}
