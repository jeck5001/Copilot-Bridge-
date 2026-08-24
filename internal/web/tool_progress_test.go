package web

import "testing"

func TestParseToolProgressDoesNotBecomeToolResult(t *testing.T) {
	p, ok := parseToolProgress(map[string]any{
		"type": "function_call_progress", "call_id": "call_1", "phase": "building",
		"message": "正在运行构建", "output": "compile package 12/40",
	})
	if !ok || p.CallID != "call_1" || p.Phase != "building" {
		t.Fatalf("bad progress: %#v", p)
	}
	if _, ok := parseToolProgress(map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"}); ok {
		t.Fatal("output parsed as progress")
	}
}
