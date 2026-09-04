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

// The two continuity blobs ride in encrypted_content behind a prefix, so the
// adapter can tell them apart and drop another provider's (base64 never has ':').
const (
	signaturePrefix = "thinking_signature:"
	redactedPrefix  = "redacted_thinking:"
)

// unsupportedFeatures are rejected with a UserError when used (modelkit.Reject).
// service_tier is here because its values do not map onto the Responses tiers.
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

// thinkingBudgets maps reasoning effort to a thinking token budget, which every
// thinking-capable model accepts; the SDK keeps no capability table (scope §1.2).
var thinkingBudgets = map[agents.ReasoningEffort]int64{
	agents.ReasoningEffortMinimal: 1024,
	agents.ReasoningEffortLow:     4096,
	agents.ReasoningEffortMedium:  16384,
	agents.ReasoningEffortHigh:    32768,
}

// convertInput translates canonical input items into Messages API turns,
// merging consecutive same-role items into one MessageParam's content blocks.
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
			// A reasoning item without encrypted content cannot be replayed: the API
			// rejects unsigned thinking blocks, and re-reads only signed ones anyway.
		default:
			return nil, agents.NewUserError(
				"anthropic: cannot translate input item %d of type %q for the Messages API — remove it from the input, or replay this history on the provider that produced it",
				i, item.Type)
		}
	}
	return messages, nil
}

// hoistLeadingSystem moves LEADING system turns' text into the top-level system
// parameter, so the first conversational message stays a user/assistant turn.
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

// messageRole maps a canonical conversational role onto a Messages API role;
// system/developer messages take the mid_conv_system path in convertInput.
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

// midConvSystemBlock wraps system-authored text in the mid_conv_system block
// (the API has no plain system input role); ok is false with no text to carry.
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
		// input_file carries base64 without a media type; the Messages document
		// source is typed application/pdf, the only file kind sent this way.
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
			// An empty text part is skipped: a text block must be at least one
			// character or the API rejects the whole next turn.
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
// item came from; ok is false when unsigned or carrying another provider's blob.
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

// convertTools translates tools and handoffs into Messages tool params — all
// locally executed function tools; strict has no equivalent and passes through.
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

// toolInputSchema maps a JSON schema object onto the typed input_schema param;
// keys it does not model ($defs, additionalProperties, …) ride along as extras.
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

// convertToolChoice maps the tool choice plus the parallel-calls setting onto
// tool_choice; ParallelToolCalls=false needs an auto choice to carry the flag.
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

// blockToItem converts one non-text content block into a canonical item (text
// is merged by convertOutput); anonymous blocks get ids from message id + index.
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
		// No server tools are requested, so a server-tool block cannot be represented
		// or replayed; dropping it silently would corrupt the conversation.
		return agents.OutputItem{}, agents.NewModelBehaviorError(
			"anthropic: response contained an unexpected content block of type %q", block.Type)
	}
}

// convertOutput converts a complete response message into canonical items:
// consecutive text blocks are ONE message, a refusal is ONE item (decisions §5.49).
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

// refusalText picks the refusal message: the response text, else stop_details,
// else a fixed line — never empty, or the runner would read it as an empty answer.
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

// statusFromStopReason maps a stop reason onto the canonical status; pause_turn
// and model_context_window_exceeded are errors (the latter one overflow recognizes).
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
// blocking path, counted as one request; the arithmetic is responseUsage's.
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
