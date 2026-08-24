package web

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

// responsesRequest is the OpenAI Responses API request subset supported by the gateway.
type responsesRequest struct {
	Model                string            `json:"model"`
	Instructions         any               `json:"instructions,omitempty"`
	Input                any               `json:"input"`
	Tools                []map[string]any  `json:"tools,omitempty"`
	ToolChoice           any               `json:"tool_choice,omitempty"`
	ParallelToolCalls    *bool             `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens      *int              `json:"max_output_tokens,omitempty"`
	Stream               bool              `json:"stream,omitempty"`
	User                 string            `json:"user,omitempty"`
	Reasoning            *reasoningConfig  `json:"reasoning,omitempty"`
	PreviousResponseID   string            `json:"previous_response_id,omitempty"`
	Conversation         any               `json:"conversation,omitempty"`
	NewConversation      bool              `json:"new_conversation,omitempty"`
	PromptCacheKey       string            `json:"prompt_cache_key,omitempty"`
	ClientMetadata       map[string]any    `json:"client_metadata,omitempty"`
	Metadata             map[string]string `json:"metadata,omitempty"`
	Background           *bool             `json:"background,omitempty"`
	ContextManagement    []any             `json:"context_management,omitempty"`
	Include              []string          `json:"include,omitempty"`
	Prompt               any               `json:"prompt,omitempty"`
	Moderation           any               `json:"moderation,omitempty"`
	Store                *bool             `json:"store,omitempty"`
	StreamOptions        map[string]any    `json:"stream_options,omitempty"`
	Temperature          *float64          `json:"temperature,omitempty"`
	Text                 map[string]any    `json:"text,omitempty"`
	TopLogprobs          *int              `json:"top_logprobs,omitempty"`
	TopP                 *float64          `json:"top_p,omitempty"`
	Truncation           string            `json:"truncation,omitempty"`
	ServiceTier          string            `json:"service_tier,omitempty"`
	SafetyIdentifier     string            `json:"safety_identifier,omitempty"`
	PromptCacheOptions   map[string]any    `json:"prompt_cache_options,omitempty"`
	PromptCacheRetention string            `json:"prompt_cache_retention,omitempty"`
	MaxToolCalls         *int              `json:"max_tool_calls,omitempty"`
}

var knownResponsesRequestFields = map[string]struct{}{
	"model": {}, "instructions": {}, "input": {}, "tools": {}, "tool_choice": {},
	"parallel_tool_calls": {}, "max_output_tokens": {}, "stream": {}, "user": {},
	"reasoning": {}, "previous_response_id": {}, "conversation": {}, "new_conversation": {},
	"prompt_cache_key": {}, "client_metadata": {}, "metadata": {}, "background": {},
	"context_management": {}, "include": {}, "prompt": {}, "moderation": {}, "store": {},
	"stream_options": {}, "temperature": {}, "text": {}, "top_logprobs": {}, "top_p": {},
	"truncation": {}, "service_tier": {}, "safety_identifier": {}, "prompt_cache_options": {},
	"prompt_cache_retention": {}, "max_tool_calls": {},
}

func (r responsesRequest) validateSemantics(fields map[string]json.RawMessage) error {
	for field := range fields {
		if _, ok := knownResponsesRequestFields[field]; !ok {
			return fmt.Errorf("unsupported parameter %q", field)
		}
	}
	if r.Background != nil && *r.Background {
		return fmt.Errorf("background=true is not supported")
	}
	if r.MaxToolCalls != nil {
		return fmt.Errorf("max_tool_calls applies to built-in tools, which this gateway does not support")
	}
	if len(r.ContextManagement) > 0 || r.Prompt != nil || r.Moderation != nil {
		return fmt.Errorf("requested Responses lifecycle or output extension is not supported")
	}
	for _, include := range r.Include {
		// Hermes requests this SDK compatibility field even though it does not
		// consume encrypted reasoning items.  The bridge cannot expose upstream
		// reasoning ciphertext, but accepting this single no-op value preserves
		// text and tool-call interoperability without silently accepting arbitrary
		// output extensions.
		if include != "reasoning.encrypted_content" {
			return fmt.Errorf("include value %q is not supported", include)
		}
	}
	if r.Temperature != nil && *r.Temperature != 1 {
		return fmt.Errorf("temperature values other than 1 are not supported")
	}
	if r.TopP != nil && *r.TopP != 1 {
		return fmt.Errorf("top_p values other than 1 are not supported")
	}
	if r.TopLogprobs != nil && *r.TopLogprobs != 0 {
		return fmt.Errorf("top_logprobs is not supported")
	}
	if r.Truncation != "" && r.Truncation != "disabled" {
		return fmt.Errorf("truncation=%q is not supported", r.Truncation)
	}
	if r.ServiceTier != "" && r.ServiceTier != "auto" && r.ServiceTier != "default" {
		return fmt.Errorf("service_tier=%q is not supported", r.ServiceTier)
	}
	if len(r.PromptCacheOptions) > 0 || r.PromptCacheRetention != "" {
		return fmt.Errorf("prompt cache policy controls are not supported")
	}
	if r.Reasoning != nil && r.Reasoning.Summary != "" && r.Reasoning.Summary != "auto" {
		return fmt.Errorf("reasoning summary mode %q is not supported", r.Reasoning.Summary)
	}
	if len(r.StreamOptions) > 0 {
		for key, value := range r.StreamOptions {
			if key != "include_obfuscation" || value != false {
				return fmt.Errorf("stream option %q is not supported", key)
			}
		}
	}
	if len(r.Text) > 0 {
		if len(r.Text) != 1 {
			return fmt.Errorf("structured text or verbosity controls are not supported")
		}
		format, ok := r.Text["format"].(map[string]any)
		if !ok || len(format) != 1 || format["type"] != "text" {
			return fmt.Errorf("structured text output is not supported")
		}
	}
	if len(r.Metadata) > 16 {
		return fmt.Errorf("metadata supports at most 16 entries")
	}
	for key, value := range r.Metadata {
		if len([]rune(key)) > 64 || len([]rune(value)) > 512 {
			return fmt.Errorf("metadata key or value exceeds the supported length")
		}
	}
	return nil
}

func (r responsesRequest) openAI() (oaiReq, error) {
	previousResponse := strings.TrimSpace(r.PreviousResponseID) != ""
	o := oaiReq{
		Model: r.Model, Stream: r.Stream, ToolChoice: r.ToolChoice, User: r.User,
		SessionKey: r.stableSessionKey(), RestorePortableHistory: previousResponse,
		ParallelToolCalls: r.ParallelToolCalls, MaxOutputTokens: r.MaxOutputTokens,
	}
	if r.MaxOutputTokens != nil && *r.MaxOutputTokens <= 0 {
		return o, fmt.Errorf("max_output_tokens must be greater than zero")
	}
	if strings.TrimSpace(r.PreviousResponseID) != "" && r.Conversation != nil {
		return o, fmt.Errorf("previous_response_id and conversation cannot both be provided")
	}
	if strings.TrimSpace(r.PreviousResponseID) != "" && r.NewConversation {
		return o, fmt.Errorf("previous_response_id and new_conversation cannot both be provided")
	}
	if r.Reasoning != nil {
		o.Reasoning = r.Reasoning
		o.ReasoningEffort = r.Reasoning.Effort
	}
	// Responses instructions are the agent's system/developer contract. They
	// are intentionally inserted on every request, including continuations:
	// previous_response_id does not inherit the previous response's
	// instructions. Silently dropping this field made Hermes/Codex lose their
	// execution policy and fall back to generic clarification responses.
	if r.Instructions != nil {
		instructions, ok := r.Instructions.(string)
		if !ok {
			return o, fmt.Errorf("instructions must be a string")
		}
		if strings.TrimSpace(instructions) != "" {
			o.Instructions = instructions
		}
	}
	switch v := r.Input.(type) {
	case string:
		if v == "" {
			return o, fmt.Errorf("input required")
		}
		o.Messages = append(o.Messages, oaiMsg{Role: "user", Content: v})
	case []any:
		for index, raw := range v {
			m, ok := raw.(map[string]any)
			if !ok {
				return o, fmt.Errorf("input item %d must be an object", index)
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "function_call_progress":
				// Progress is deliberately not converted into an assistant/tool
				// message. It is transport metadata from a long-running client-side
				// executor and must not trigger a model turn or tool completion.
				if _, ok := parseToolProgress(m); !ok {
					return o, fmt.Errorf("invalid function_call_progress")
				}
				continue
			case "function_call_output":
				if previousResponse {
					o.AllowResponsesToolContinuation = true
				}
				id, _ := m["call_id"].(string)
				output, exists := m["output"]
				if strings.TrimSpace(id) == "" || !exists || output == nil {
					return o, fmt.Errorf("function_call_output requires call_id and non-null output")
				}
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: output})
			case "function_call":
				id, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				args := m["arguments"]
				if s, ok := args.(string); ok {
					var x any
					if json.Unmarshal([]byte(s), &x) == nil {
						args = x
					}
				}
				call := map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": mustJSON(args)}}
				// Responses represents parallel calls as adjacent output items. Chat
				// Completions represents the same calls in one assistant turn.
				if n := len(o.Messages); n > 0 && o.Messages[n-1].Role == "assistant" && o.Messages[n-1].Content == nil && len(o.Messages[n-1].ToolCalls) > 0 {
					o.Messages[n-1].ToolCalls = append(o.Messages[n-1].ToolCalls, call)
				} else {
					o.Messages = append(o.Messages, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{call}})
				}
			case "", "message":
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				o.Messages = append(o.Messages, oaiMsg{Role: role, Content: m["content"]})
			default:
				return o, fmt.Errorf("unsupported input item type %q", typ)
			}
		}
	default:
		return o, fmt.Errorf("input must be string or array")
	}
	for _, t := range r.Tools {
		if typ, _ := t["type"].(string); typ != "function" {
			return o, fmt.Errorf("unsupported tool type %q", typ)
		}
		// Accept both tool shapes: the flat Responses API form
		// {"type":"function","name":...,"parameters":...} and the nested Chat
		// Completions form {"type":"function","function":{"name":...}}. A
		// nested definition previously produced name:null tools, which made
		// the tool router silently report no callable tools.
		fn, _ := t["function"].(map[string]any)
		name := firstNonEmptyString(t["name"], fn["name"])
		if name == "" {
			return o, fmt.Errorf("function tool name required")
		}
		description := firstNonEmptyString(t["description"], fn["description"])
		parameters, hasParams := firstMap(t["parameters"], fn["parameters"])
		f := map[string]any{"name": name}
		if description != "" {
			f["description"] = description
		}
		if hasParams {
			f["parameters"] = parameters
		}
		b, _ := json.Marshal(f)
		o.Tools = append(o.Tools, chathub.Tool{Type: "function", Function: b})
	}
	return o, nil
}

func withRequestInstructions(messages []oaiMsg, instructions string) []oaiMsg {
	if strings.TrimSpace(instructions) == "" {
		return messages
	}
	out := make([]oaiMsg, 0, len(messages)+1)
	out = append(out, oaiMsg{Role: "developer", Content: instructions})
	return append(out, messages...)
}

// firstNonEmptyString returns the first value that is a non-empty string.
func firstNonEmptyString(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// firstMap returns the first value that is a non-nil map.
func firstMap(values ...any) (map[string]any, bool) {
	for _, v := range values {
		if m, ok := v.(map[string]any); ok && m != nil {
			return m, true
		}
	}
	return nil, false
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}
type anthropicRequest struct {
	Model      string             `json:"model"`
	System     any                `json:"system,omitempty"`
	Messages   []anthropicMessage `json:"messages"`
	Tools      []anthropicTool    `json:"tools,omitempty"`
	ToolChoice any                `json:"tool_choice,omitempty"`
	Stream     bool               `json:"stream,omitempty"`
	MaxTokens  int                `json:"max_tokens,omitempty"`
}

func (r anthropicRequest) openAI() (oaiReq, error) {
	o := oaiReq{Model: r.Model, Stream: r.Stream}
	if r.System != nil {
		o.Messages = append(o.Messages, oaiMsg{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		if s, ok := m.Content.(string); ok {
			o.Messages = append(o.Messages, oaiMsg{Role: m.Role, Content: s})
			continue
		}
		blocks, ok := m.Content.([]any)
		if !ok {
			return o, fmt.Errorf("invalid anthropic content")
		}
		var text []any
		var calls []map[string]any
		for _, raw := range blocks {
			b, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := b["type"].(string)
			switch typ {
			case "text":
				text = append(text, b)
			case "tool_use":
				calls = append(calls, map[string]any{"id": b["id"], "type": "function", "function": map[string]any{"name": b["name"], "arguments": mustJSON(b["input"])}})
			case "tool_result":
				id, _ := b["tool_use_id"].(string)
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: b["content"]})
			}
		}
		if len(text) > 0 || len(calls) > 0 {
			o.Messages = append(o.Messages, oaiMsg{Role: m.Role, Content: text, ToolCalls: calls})
		}
	}
	for _, t := range r.Tools {
		f := map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}
		b, _ := json.Marshal(f)
		o.Tools = append(o.Tools, chathub.Tool{Type: "function", Function: b})
	}
	if c, ok := r.ToolChoice.(map[string]any); ok {
		switch c["type"] {
		case "auto":
			o.ToolChoice = "auto"
		case "any":
			o.ToolChoice = "required"
		case "none":
			o.ToolChoice = "none"
		case "tool":
			o.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": c["name"]}}
		}
	}
	return o, nil
}
