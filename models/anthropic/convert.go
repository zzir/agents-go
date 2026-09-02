package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	ant "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/modelkit"
)

// The adapter stores its two opaque continuity blobs — thinking signatures
// and redacted thinking — in the reasoning item's encrypted_content, the one
// canonical slot that survives session storage. Each carries a prefix, for
// two reasons: the adapter must tell its own two uses apart on the way back
// in, and a blob WITHOUT a recognized prefix is provably not ours (another
// provider's encrypted reasoning, replayed cross-provider) and is dropped
// instead of being sent as a bogus signature the API would reject with a
// message that names none of this. Signatures are base64-ish and never
// contain ':', so the prefixes cannot collide with payloads.
const (
	signaturePrefix = "thinking_signature:"
	redactedPrefix  = "redacted_thinking:"
)

// unsupportedFeatures are the request features this backend has no equivalent
// for. Each is rejected with a UserError when actually used (modelkit.Reject);
// see Capabilities for the static view.
//
// service_tier is here although Anthropic has one: its values (auto /
// standard_only) do not correspond to the Responses tiers (default / flex /
// priority), and guessing a mapping would silently buy a different QoS than
// the one configured.
var unsupportedFeatures = []modelkit.Feature{
	modelkit.FeatureTruncation,
	modelkit.FeatureVerbosity,
	modelkit.FeatureServiceTier,
	modelkit.FeatureStore,
	modelkit.FeaturePromptCacheRetention,
	modelkit.FeaturePromptCacheKey,
	modelkit.FeaturePromptCacheOptions,
	modelkit.FeatureContextManagement,
	modelkit.FeatureResponseInclude,
	modelkit.FeatureTopLogprobs,
	modelkit.FeatureReasoningSummary,
	modelkit.FeaturePreviousResponseID,
	modelkit.FeatureConversationID,
	modelkit.FeaturePrompt,
}

// Capabilities declares this adapter's unsupported request features, for
// hosting layers that surface limits ahead of a run. The enforced truth is
// the per-call rejection.
func Capabilities() modelkit.Capabilities {
	return modelkit.Capabilities{Unsupported: unsupportedFeatures}
}

// DefaultMaxTokens is used when the request does not set MaxTokens. The
// Messages API requires max_tokens on every call, so "unset" needs a value;
// requiring every caller to pick one would make the provider unusable as a
// drop-in. When thinking is enabled the default grows to keep the budget
// below the cap (see thinkingBudget).
const DefaultMaxTokens int64 = 8192

// thinkingBudgets maps the canonical reasoning effort to a thinking token
// budget. Budgets rather than the native effort parameter because budgets
// work on every thinking-capable Claude model, while effort/adaptive thinking
// exists only on the newest ones — and this SDK keeps no model-capability
// tables to know which is which (scope §1.2).
var thinkingBudgets = map[agents.ReasoningEffort]int64{
	agents.ReasoningEffortMinimal: 1024,
	agents.ReasoningEffortLow:     4096,
	agents.ReasoningEffortMedium:  16384,
	agents.ReasoningEffortHigh:    32768,
}

// convertInput translates canonical input items into Messages API turns.
//
// Consecutive items of the same role are merged into one MessageParam
// client-side: a canonical history interleaves reasoning / message /
// function_call as separate items, and the Messages API wants them as content
// blocks of a single assistant turn (thinking first, then text, then
// tool_use — the order the canonical history already has them in).
func convertInput(items []agents.InputItem) ([]ant.MessageParam, error) {
	parsed, err := modelkit.ParseInput(items)
	if err != nil {
		return nil, err
	}
	var messages []ant.MessageParam
	appendBlocks := func(role ant.MessageParamRole, blocks ...ant.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if n := len(messages); n > 0 && messages[n-1].Role == role {
			messages[n-1].Content = append(messages[n-1].Content, blocks...)
			return
		}
		messages = append(messages, ant.MessageParam{Role: role, Content: blocks})
	}

	for i, item := range parsed {
		switch item.Type {
		case "message":
			if item.Role == "system" || item.Role == "developer" {
				block, ok, err := midConvSystemBlock(item.Parts)
				if err != nil {
					return nil, fmt.Errorf("input item %d: %w", i, err)
				}
				if ok {
					appendBlocks(ant.MessageParamRoleSystem, block)
				}
				continue
			}
			role, err := messageRole(item.Role)
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", i, err)
			}
			blocks, err := messageBlocks(item.Parts)
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", i, err)
			}
			appendBlocks(role, blocks...)
		case "function_call":
			args := item.Arguments
			if args == "" {
				args = "{}"
			}
			appendBlocks(ant.MessageParamRoleAssistant,
				ant.NewToolUseBlock(item.CallID, json.RawMessage(args), item.Name))
		case "function_call_output":
			block, err := toolResultBlock(item)
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", i, err)
			}
			appendBlocks(ant.MessageParamRoleUser, block)
		case "reasoning":
			block, ok := thinkingBlock(item)
			if ok {
				appendBlocks(ant.MessageParamRoleAssistant, block)
			}
			// A reasoning item without encrypted content cannot be replayed:
			// the API rejects thinking blocks that carry no signature. Dropping
			// it loses nothing the backend would use — it re-reads only signed
			// thinking — and matches how the API itself ignores prior-turn
			// thinking.
		default:
			return nil, agents.NewUserError(
				"anthropic: cannot translate input item %d of type %q for the Messages API — remove it from the input, or replay this history on the provider that produced it",
				i, item.Type)
		}
	}
	return messages, nil
}

// hoistLeadingSystem moves LEADING system turns' text out of messages and
// returns it for the top-level system parameter. The mid_conv_system block is
// specified for system instructions that appear MID-conversation; a
// compaction summary rendered at the very front of the input is
// top-of-conversation system content, and hoisting it keeps the first
// conversational message a user/assistant turn — the shape the Messages API
// documents.
func hoistLeadingSystem(messages []ant.MessageParam) (rest []ant.MessageParam, system []ant.TextBlockParam) {
	for len(messages) > 0 && messages[0].Role == ant.MessageParamRoleSystem {
		for _, block := range messages[0].Content {
			if mc := block.OfMidConvSystem; mc != nil {
				system = append(system, mc.Content...)
			}
		}
		messages = messages[1:]
	}
	return messages, system
}

// messageRole maps a canonical conversational role onto a Messages API role.
// System/developer messages never reach here — they take the mid_conv_system
// path in convertInput.
func messageRole(role string) (ant.MessageParamRole, error) {
	switch role {
	case "user":
		return ant.MessageParamRoleUser, nil
	case "assistant":
		return ant.MessageParamRoleAssistant, nil
	default:
		return "", agents.NewUserError("anthropic: message role %q has no Messages API equivalent", role)
	}
}

// midConvSystemBlock wraps system-authored text (a compaction summary, a
// middleware injection — "developer" is the Responses spelling of the same
// thing) in the mid_conv_system block the Messages API defines for system
// instructions that appear mid-conversation. NOT a plain text block under a
// system role: the API's own docs state there is no plain "system" role for
// input messages — a system turn carries structured system blocks, and the
// SDK's tests use exactly this role+block pairing. ok is false for a message
// with no text to carry.
func midConvSystemBlock(parts []modelkit.Part) (ant.ContentBlockParamUnion, bool, error) {
	var texts []ant.TextBlockParam
	for _, p := range parts {
		if !p.IsText() {
			return ant.ContentBlockParamUnion{}, false, agents.NewUserError(
				"anthropic: system message content part %q has no Messages API equivalent — system turns are text-only", p.Type)
		}
		if p.Text != "" {
			texts = append(texts, ant.TextBlockParam{Text: p.Text})
		}
	}
	if len(texts) == 0 {
		return ant.ContentBlockParamUnion{}, false, nil
	}
	return ant.NewMidConvSystemBlock(texts), true, nil
}

// messageBlocks converts message content parts into content blocks.
func messageBlocks(parts []modelkit.Part) ([]ant.ContentBlockParamUnion, error) {
	var blocks []ant.ContentBlockParamUnion
	for _, p := range parts {
		switch {
		case p.IsText():
			if p.Text != "" {
				blocks = append(blocks, ant.NewTextBlock(p.Text))
			}
		case p.Type == "refusal":
			// Dropped, not replayed as assistant text: a refusal is not an
			// answer the model gave (decisions §5.49).
		case p.Type == "input_image":
			block, err := imageBlock(p)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		case p.Type == "input_file":
			block, err := documentBlock(p)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		default:
			return nil, agents.NewUserError("anthropic: message content part %q has no Messages API equivalent", p.Type)
		}
	}
	return blocks, nil
}

func imageBlock(p modelkit.Part) (ant.ContentBlockParamUnion, error) {
	switch {
	case p.FileID != "":
		return ant.ContentBlockParamUnion{}, agents.NewUserError(
			"anthropic: image content referencing an OpenAI file id cannot be sent to the Messages API — inline the image as a data: URL instead")
	case p.ImageURL == "":
		return ant.ContentBlockParamUnion{}, agents.NewUserError("anthropic: image content has no image_url")
	}
	if mediaType, data, ok := parseDataURL(p.ImageURL); ok {
		return ant.NewImageBlockBase64(mediaType, data), nil
	}
	return ant.NewImageBlock(ant.URLImageSourceParam{URL: p.ImageURL}), nil
}

func documentBlock(p modelkit.Part) (ant.ContentBlockParamUnion, error) {
	var block ant.ContentBlockParamUnion
	switch {
	case p.FileData != "":
		// The Responses input_file carries base64 without a media type; the
		// Messages API document source is typed application/pdf, which is also
		// the only file kind the Responses side sends this way.
		block = ant.NewDocumentBlock(ant.Base64PDFSourceParam{Data: p.FileData})
	case p.FileURL != "":
		block = ant.NewDocumentBlock(ant.URLPDFSourceParam{URL: p.FileURL})
	default:
		return block, agents.NewUserError(
			"anthropic: file content referencing an OpenAI file id cannot be sent to the Messages API — inline the file as base64 file_data or a file_url instead")
	}
	if p.Filename != "" && block.OfDocument != nil {
		block.OfDocument.Title = ant.String(p.Filename)
	}
	return block, nil
}

// toolResultBlock converts a function_call_output into a tool_result block.
func toolResultBlock(item modelkit.Item) (ant.ContentBlockParamUnion, error) {
	result := ant.ToolResultBlockParam{ToolUseID: item.CallID}
	for _, p := range item.Output {
		switch {
		case p.IsText():
			// An empty text part is skipped, not sent: a side-effect tool
			// returning "" is routine, an empty tool_result content list is
			// legal, but a text block must be at least one character — the
			// API would reject the whole next turn over it.
			if p.Text == "" {
				continue
			}
			result.Content = append(result.Content, ant.ToolResultBlockParamContentUnion{
				OfText: &ant.TextBlockParam{Text: p.Text},
			})
		case p.Type == "input_image":
			block, err := imageBlock(p)
			if err != nil {
				return ant.ContentBlockParamUnion{}, err
			}
			result.Content = append(result.Content, ant.ToolResultBlockParamContentUnion{
				OfImage: block.OfImage,
			})
		case p.Type == "input_file":
			block, err := documentBlock(p)
			if err != nil {
				return ant.ContentBlockParamUnion{}, err
			}
			result.Content = append(result.Content, ant.ToolResultBlockParamContentUnion{
				OfDocument: block.OfDocument,
			})
		default:
			return ant.ContentBlockParamUnion{}, agents.NewUserError(
				"anthropic: tool result content part %q has no Messages API equivalent", p.Type)
		}
	}
	return ant.ContentBlockParamUnion{OfToolResult: &result}, nil
}

// thinkingBlock rebuilds the thinking / redacted_thinking block a reasoning
// item was synthesized from. ok is false for an item with nothing this
// backend can replay: no encrypted content (unsigned), or a blob without the
// adapter's prefix — another provider's reasoning, which only that provider
// can read.
func thinkingBlock(item modelkit.Item) (ant.ContentBlockParamUnion, bool) {
	enc := item.EncryptedContent
	if data, ok := strings.CutPrefix(enc, redactedPrefix); ok {
		return ant.NewRedactedThinkingBlock(data), true
	}
	sig, ok := strings.CutPrefix(enc, signaturePrefix)
	if !ok {
		return ant.ContentBlockParamUnion{}, false
	}
	texts := item.ContentTexts
	if len(texts) == 0 {
		texts = item.SummaryTexts
	}
	return ant.NewThinkingBlock(sig, strings.Join(texts, "\n\n")), true
}

// parseDataURL splits a data: URL into media type and base64 payload.
func parseDataURL(url string) (mediaType, data string, ok bool) {
	rest, found := strings.CutPrefix(url, "data:")
	if !found {
		return "", "", false
	}
	mediaType, data, found = strings.Cut(rest, ";base64,")
	if !found || mediaType == "" || data == "" {
		return "", "", false
	}
	return mediaType, data, true
}

// convertTools translates the SDK's tools and handoffs into Messages API tool
// params. As on the OpenAI side, every tool is a locally executed function
// tool; the strict flag has no Messages equivalent, but a strict schema is
// still a valid schema, so it passes through unchanged.
func convertTools(tools []*agents.Tool, handoffs []agents.Handoff) []ant.ToolUnionParam {
	out := make([]ant.ToolUnionParam, 0, len(tools)+len(handoffs))
	for _, t := range tools {
		out = append(out, functionToolParam(t.Name, t.Description, t.ParamsJSONSchema))
	}
	for _, h := range handoffs {
		out = append(out, functionToolParam(h.ToolName, h.ToolDescription, h.InputJSONSchema))
	}
	return out
}

func functionToolParam(name, description string, schema map[string]any) ant.ToolUnionParam {
	tool := ant.ToolParam{Name: name, InputSchema: toolInputSchema(schema)}
	if description != "" {
		tool.Description = ant.String(description)
	}
	return ant.ToolUnionParam{OfTool: &tool}
}

// toolInputSchema maps a JSON schema object onto the typed input_schema param.
// Keys the param does not model (additionalProperties, $defs, …) ride along as
// extra fields so a strict-mode schema arrives intact.
func toolInputSchema(schema map[string]any) ant.ToolInputSchemaParam {
	is := ant.ToolInputSchemaParam{Properties: map[string]any{}}
	extras := map[string]any{}
	for k, v := range schema {
		switch k {
		case "type":
			// The param's type is the constant "object" — which is the only
			// valid value for a function tool schema anyway.
		case "properties":
			is.Properties = v
		case "required":
			if req, ok := stringSlice(v); ok {
				is.Required = req
			} else {
				extras[k] = v
			}
		default:
			extras[k] = v
		}
	}
	if len(extras) > 0 {
		is.ExtraFields = extras
	}
	return is
}

func stringSlice(v any) ([]string, bool) {
	switch vv := v.(type) {
	case []string:
		return vv, true
	case []any:
		out := make([]string, len(vv))
		for i, e := range vv {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

// convertToolChoice maps the canonical tool choice plus the parallel-calls
// setting onto the Messages tool_choice union. Anthropic hangs "may the model
// issue several calls at once" off tool_choice, so a ParallelToolCalls=false
// with no explicit choice still needs an auto choice to carry the flag.
func convertToolChoice(choice agents.ToolChoice, parallel *bool, hasTools bool) (ant.ToolChoiceUnionParam, bool) {
	// Omitted unless parallel calls are explicitly disabled: true is already
	// the provider default, and sending it would be noise.
	var disable param.Opt[bool]
	if parallel != nil && !*parallel {
		disable = ant.Bool(true)
	}
	switch choice {
	case "":
		if parallel != nil && !*parallel && hasTools {
			return ant.ToolChoiceUnionParam{OfAuto: &ant.ToolChoiceAutoParam{DisableParallelToolUse: disable}}, true
		}
		return ant.ToolChoiceUnionParam{}, false
	case agents.ToolChoiceAuto:
		return ant.ToolChoiceUnionParam{OfAuto: &ant.ToolChoiceAutoParam{DisableParallelToolUse: disable}}, true
	case agents.ToolChoiceRequired:
		return ant.ToolChoiceUnionParam{OfAny: &ant.ToolChoiceAnyParam{DisableParallelToolUse: disable}}, true
	case agents.ToolChoiceNone:
		return ant.ToolChoiceUnionParam{OfNone: &ant.ToolChoiceNoneParam{}}, true
	default:
		return ant.ToolChoiceUnionParam{
			OfTool: &ant.ToolChoiceToolParam{Name: string(choice), DisableParallelToolUse: disable},
		}, true
	}
}

// blockToItem converts one non-text response content block into a canonical
// output item (text blocks are merged by convertOutput). Item ids are
// synthesized from the message id and block index for blocks the API leaves
// anonymous; a tool_use block's own id doubles as both item id and call id.
func blockToItem(msgID string, index int, block ant.ContentBlockUnion) (agents.OutputItem, error) {
	switch block.Type {
	case "tool_use":
		return modelkit.FunctionCallItem(block.ID, block.ID, block.Name, string(block.Input))
	case "thinking":
		enc := ""
		if block.Signature != "" {
			enc = signaturePrefix + block.Signature
		}
		return modelkit.ReasoningItem(blockItemID(msgID, index), block.Thinking, enc)
	case "redacted_thinking":
		return modelkit.ReasoningItem(blockItemID(msgID, index), "", redactedPrefix+block.Data)
	default:
		// No server tools are ever requested, so no server-tool block should
		// ever arrive; one that does cannot be represented (or replayed) and
		// silently dropping it would corrupt the conversation.
		return agents.OutputItem{}, agents.NewModelBehaviorError(
			"anthropic: response contained an unexpected content block of type %q", block.Type)
	}
}

// convertOutput converts a complete response message into canonical items
// (decisions §5.49): consecutive text blocks become ONE message item with a
// part each, since the runner reads only a turn's last message; a refusal
// terminal produces ONE refusal item and nothing else, so the partially
// generated tool_use blocks a refused response may carry never execute.
func convertOutput(msg *ant.Message) ([]agents.OutputItem, error) {
	if msg.StopReason == ant.StopReasonRefusal {
		item, err := modelkit.RefusalItem(blockItemID(msg.ID, 0), refusalText(msg))
		if err != nil {
			return nil, err
		}
		return []agents.OutputItem{item}, nil
	}
	items := make([]agents.OutputItem, 0, len(msg.Content))
	for i := 0; i < len(msg.Content); {
		var item agents.OutputItem
		var err error
		if msg.Content[i].Type == "text" {
			start := i
			var texts []string
			for ; i < len(msg.Content) && msg.Content[i].Type == "text"; i++ {
				texts = append(texts, msg.Content[i].Text)
			}
			item, err = modelkit.MessageItem(blockItemID(msg.ID, start), texts...)
		} else {
			item, err = blockToItem(msg.ID, i, msg.Content[i])
			i++
		}
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// refusalText picks the refusal message the run surfaces: the response's own
// text, else stop_details' explanation, else a fixed line — never empty, or
// the runner's refusal detection (which keys on a non-empty refusal part)
// would read the refusal as a successful empty answer.
func refusalText(msg *ant.Message) string {
	texts := make([]string, 0, len(msg.Content))
	for _, block := range msg.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			texts = append(texts, block.Text)
		}
	}
	if joined := strings.TrimSpace(strings.Join(texts, "\n")); joined != "" {
		return joined
	}
	if e := strings.TrimSpace(msg.StopDetails.Explanation); e != "" {
		return e
	}
	return "The model refused to respond."
}

// statusFromStopReason maps a stop reason onto the canonical response status.
// The two reasons that are really failures come back as errors: pause_turn
// cannot occur without server tools, and model_context_window_exceeded means
// the conversation no longer fits — resending it unchanged would stop at the
// same wall, so it surfaces as an error the overflow policy recognizes and
// answers with compact-and-retry.
func statusFromStopReason(reason ant.StopReason) (status, incompleteReason string, err error) {
	switch reason {
	case "", ant.StopReasonEndTurn, ant.StopReasonToolUse, ant.StopReasonStopSequence, ant.StopReasonRefusal:
		return "completed", "", nil
	case ant.StopReasonMaxTokens:
		return "incomplete", "max_output_tokens", nil
	case ant.StopReasonModelContextWindowExceeded:
		return "", "", agents.NewModelBehaviorError(
			"anthropic: response stopped: model_context_window_exceeded — the conversation no longer fits the model's context window")
	default:
		return "", "", agents.NewModelBehaviorError("anthropic: unexpected stop_reason %q", reason)
	}
}

// usageFromMessage maps Messages usage onto canonical accounting for the
// blocking path, counted as one request.
//
// The arithmetic — Anthropic reports uncached, cache-read and cache-write
// input separately, canonical InputTokens is their sum (Responses semantics:
// the total, with the cache numbers as informational subsets) — is
// responseUsage's, in stream.go. This only widens that result, so the blocking
// and streaming paths cannot report different totals for the same message.
func usageFromMessage(u ant.Usage) *agents.Usage {
	ru := responseUsage(u)
	return &agents.Usage{
		Requests:     1,
		InputTokens:  ru.InputTokens,
		OutputTokens: ru.OutputTokens,
		TotalTokens:  ru.TotalTokens,
		InputTokensDetails: agents.InputTokensDetails{
			CachedTokens:     ru.CachedTokens,
			CacheWriteTokens: ru.CacheWriteTokens,
		},
		OutputTokensDetails: agents.OutputTokensDetails{ReasoningTokens: ru.ReasoningTokens},
	}
}
