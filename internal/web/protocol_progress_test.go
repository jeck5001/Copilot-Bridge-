package web

import "testing"

func TestResponsesInputProgressIsIgnoredByToolLedger(t *testing.T) {
	r := responsesRequest{Input: []any{
		map[string]any{"type": "function_call", "call_id": "c1", "name": "workspace_shell", "arguments": `{"command":"go build ./..."}`},
		map[string]any{"type": "function_call_progress", "call_id": "c1", "phase": "building", "message": "正在构建"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 2 {
		t.Fatalf("progress should be transport-only, got %d messages", len(o.Messages))
	}
	if o.Messages[1].Role != "tool" || o.Messages[1].ToolCallID != "c1" {
		t.Fatalf("bad converted output: %#v", o.Messages)
	}
}
