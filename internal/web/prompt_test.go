package web

import (
	"strings"
	"testing"
)

func TestFlattenPromptUsesCapturedM365AdditionalContextShape(t *testing.T) {
	prompt, _ := flattenPromptMessages([]oaiMsg{
		{Role: "developer", Content: "Answer in exactly two sentences."},
		{Role: "user", Content: "Explain health checks."},
	}, nil)
	want := "System instructions:\nAnswer in exactly two sentences.\n\n---\n\nExplain health checks."
	if prompt != want {
		t.Fatalf("prompt shape mismatch\n got: %q\nwant: %q", prompt, want)
	}
	if strings.Contains(prompt, "[system]") || strings.Contains(prompt, "[developer]") || strings.Contains(prompt, "AUTHORITATIVE") {
		t.Fatalf("jailbreak-like role wrapper leaked into M365 prompt: %q", prompt)
	}
}

func TestFlattenPromptLeavesSimpleUserTurnPlain(t *testing.T) {
	prompt, _ := flattenPromptMessages([]oaiMsg{{Role: "user", Content: "hello"}}, nil)
	if prompt != "hello" {
		t.Fatalf("simple user prompt=%q", prompt)
	}
}

func TestFlattenPromptLabelsCurrentToolEvidence(t *testing.T) {
	prompt, _ := flattenPromptMessages([]oaiMsg{
		{Role: "developer", Content: "Use successful tool evidence."},
		{Role: "user", Content: "Check the service."},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_1", "type": "function"}}},
		{Role: "tool", ToolCallID: "call_1", Content: `{"ok":true}`},
	}, nil)
	for _, marker := range []string{"System instructions:", "Conversation transcript and current tool evidence:", "[assistant tool_calls]", "[tool result id=call_1]", `{"ok":true}`} {
		if !strings.Contains(prompt, marker) {
			t.Fatalf("tool-evidence prompt missing %q: %q", marker, prompt)
		}
	}
}
