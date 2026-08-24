package chathub

import "testing"

func TestClassifyUpdateMessages(t *testing.T) {
	got := classifyUpdateMessages([]any{
		map[string]any{"author": "bot", "text": "我先查一下", "messageType": ""},
		map[string]any{"messageType": "Progress", "contentType": "SearchResults", "text": "正在搜索"},
		map[string]any{"toolName": "web_search", "arguments": map[string]any{"query": "golang"}},
	})
	if len(got) != 3 || got[0].Kind != "text" || got[1].Kind != "progress" || got[2].Kind != "tool" {
		t.Fatalf("unexpected events: %#v", got)
	}
	if got[2].ToolName != "web_search" || len(got[2].Arguments) == 0 {
		t.Fatalf("tool fields missing: %#v", got[2])
	}
}

func TestExtractToolEventsNestedAndDeduped(t *testing.T) {
	seen := map[string]bool{}
	arg := map[string]any{"plugin": map[string]any{"functionName": "list_files", "functionArguments": map[string]any{"path": "."}}}
	got := extractToolEvents([]any{arg, arg}, seen)
	if len(got) != 1 || got[0].ToolName != "list_files" {
		t.Fatalf("unexpected nested tools: %#v", got)
	}
}
