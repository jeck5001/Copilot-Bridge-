package web

import (
	"encoding/json"
	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
	"testing"
)

func TestNativeToolCallsOnlyFromFrame(t *testing.T) {
	tools := []chathub.Tool{{Type: "function", Function: json.RawMessage(`{"name":"get_current_time","parameters":{"type":"object"}}`)}}
	events := []json.RawMessage{json.RawMessage(`{"type":1,"target":"plugin","arguments":{"pluginName":"get_current_time","arguments":{"timezone":"Asia/Shanghai"}}}`)}
	c := nativeToolCalls(events, tools)
	if len(c) != 1 || c[0].Name != "get_current_time" || string(c[0].Arguments) != "{"+`"timezone":"Asia/Shanghai"`+"}" {
		t.Fatalf("%+v", c)
	}
	if len(nativeToolCalls([]json.RawMessage{json.RawMessage(`{"text":"现在几点"}`)}, tools)) != 0 {
		t.Fatal("inferred a tool call")
	}
}

func TestNativeToolCallsValidateSchemaAndDeduplicate(t *testing.T) {
	tools := []chathub.Tool{{Type: "function", Function: json.RawMessage(`{"name":"get_weather","parameters":{"type":"object","required":["city"],"properties":{"city":{"type":"string"}}}}`)}}
	events := []json.RawMessage{
		json.RawMessage(`{"pluginName":"get_weather","arguments":{"city":2}}`),
		json.RawMessage(`{"pluginName":"get_weather","arguments":{"city":"Beijing"}}`),
		json.RawMessage(`{"pluginName":"get_weather","arguments":{"city":"Beijing"}}`),
		json.RawMessage(`{"id":"get_weather","arguments":{"city":"must-not-infer-from-id"}}`),
	}
	calls := nativeToolCalls(events, tools)
	if len(calls) != 1 || calls[0].Name != "get_weather" || string(calls[0].Arguments) != `{"city":"Beijing"}` {
		t.Fatalf("native schema validation/deduplication failed: %+v", calls)
	}
}
