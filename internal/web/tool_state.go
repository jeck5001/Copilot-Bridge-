package web

import "fmt"

// validateToolConversation enforces the OpenAI tool protocol without making
// assumptions about what a tool does. Every assistant call must be followed by
// exactly one matching tool result before another model turn is requested.
func validateToolConversation(messages []oaiMsg) error {
	pending := map[string]bool{}
	completed := map[string]bool{}
	for i, m := range messages {
		if len(pending) > 0 && m.Role != "tool" {
			return fmt.Errorf("tool results must immediately follow assistant calls before %s message at index %d", m.Role, i)
		}
		switch m.Role {
		case "assistant":
			if len(pending) > 0 {
				return fmt.Errorf("tool results missing before assistant message at index %d", i)
			}
			for _, call := range m.ToolCalls {
				id, _ := call["id"].(string)
				if id == "" {
					return fmt.Errorf("assistant tool call missing id at index %d", i)
				}
				if pending[id] || completed[id] {
					return fmt.Errorf("duplicate tool call id: %s", id)
				}
				pending[id] = true
			}
		case "tool":
			if m.ToolCallID == "" {
				return fmt.Errorf("tool_call_id required at index %d", i)
			}
			if !pending[m.ToolCallID] {
				return fmt.Errorf("unexpected tool result: %s", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
			completed[m.ToolCallID] = true
		}
	}
	if len(pending) > 0 {
		for id := range pending {
			return fmt.Errorf("missing tool result for tool_call_id: %s", id)
		}
	}
	return nil
}

// allowResponsesToolContinuation accepts the Responses API shape where a
// previous_response_id carries the assistant function call and the current
// request contains only function_call_output items. That call/result pair is
// valid across Responses turns even though the stateless converted Chat
// request does not repeat the prior assistant tool_calls item.
func allowResponsesToolContinuation(body oaiReq) bool {
	if !body.AllowResponsesToolContinuation || len(body.Messages) == 0 {
		return false
	}
	hasToolResult := false
	for _, message := range body.Messages {
		switch message.Role {
		case "tool":
			hasToolResult = true
		case "user":
			// Some clients resend a short user acknowledgement with the tool
			// output; it remains safe because no assistant call is introduced.
		default:
			return false
		}
	}
	return hasToolResult
}
