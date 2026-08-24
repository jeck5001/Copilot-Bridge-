package chathub

import "encoding/json"

type Tool struct {
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function,omitempty"`
}

// clientPlugins returns the plugins array for a chat turn. The real
// m365.cloud.microsoft client sends exactly one built-in plugin (Bing web
// search) when no Copilot Studio agent is attached; it never advertises
// client-side tools here. Fabricating one plugin entry per OpenAI tool made
// the request look like a large tool block — the shape that trips the
// upstream Disengaged safety filter (~12 tool entries is borderline, coding
// agents ship 9-15). Tool definitions reach the model through the prompt
// injection in toolProtocolPrompt instead.
func clientPlugins(tools []Tool) []any {
	_ = tools // kept for signature stability; plugins are fixed regardless of tool count
	return []any{map[string]any{"Id": "BingWebSearch", "Source": "BuiltIn"}}
}
