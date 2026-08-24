package web

import "testing"

func TestFencedToolCalls(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "get_current_time", "parameters": map[string]any{"type": "object"}}}}
	calls := fencedToolCalls("```get_current_time\n{}\n```", tools, "auto")
	if len(calls) != 1 || calls[0].Name != "get_current_time" || string(calls[0].Arguments) != "{}" {
		t.Fatalf("%v", calls)
	}
}

func TestFencedToolCallsRejectUnknown(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "get_current_time"}}}
	if got := fencedToolCalls("```shell\n{}\n```", tools, "auto"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := fencedToolCalls("```get_current_time\n{}\n```", tools, "none"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestFencedToolCallsValidateSchemaAndDeduplicate(t *testing.T) {
	tools := testTools()
	text := "```get_weather\n{\"city\":2}\n```\n```get_weather\n{\"city\":\"Beijing\"}\n```\n```get_weather\n{\"city\":\"Beijing\"}\n```"
	calls := fencedToolCalls(text, tools, "auto")
	if len(calls) != 1 || calls[0].Name != "get_weather" || string(calls[0].Arguments) != `{"city":"Beijing"}` {
		t.Fatalf("fallback schema validation/deduplication failed: %+v", calls)
	}
}
