package web

import (
	"encoding/json"

	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

type nativeToolCandidate struct {
	name string
	args any
}

// nativeToolCalls converts only tool invocations actually present in ChatHub
// frames. It never infers a call from the user's text and never makes a second request.
func nativeToolCalls(events []json.RawMessage, tools []chathub.Tool) []detectedToolCall {
	toolMaps := make([]map[string]any, 0, len(tools))
	allowed := map[string]bool{}
	for _, t := range tools {
		var f map[string]any
		if json.Unmarshal(t.Function, &f) == nil {
			name, _ := f["name"].(string)
			if name == "" {
				continue
			}
			allowed[name] = true
			toolMaps = append(toolMaps, map[string]any{"type": t.Type, "function": f})
		}
	}
	var candidates []nativeToolCandidate
	for _, raw := range events {
		var v any
		if json.Unmarshal(raw, &v) == nil {
			walkNative(v, allowed, &candidates)
		}
	}
	var out []detectedToolCall
	for _, raw := range candidates {
		call, ok := validatedFallbackToolCall(raw.name, raw.args, toolMaps, "auto", len(out))
		if ok {
			out = appendUniqueToolCall(out, call)
		}
	}
	return out
}
func walkNative(v any, allowed map[string]bool, out *[]nativeToolCandidate) {
	switch x := v.(type) {
	case []any:
		for _, y := range x {
			walkNative(y, allowed, out)
		}
	case map[string]any:
		name := ""
		for _, k := range []string{"name", "toolName", "pluginName", "functionName"} {
			if s, ok := x[k].(string); ok && allowed[s] {
				name = s
				break
			}
		}
		if name != "" {
			var a any
			for _, k := range []string{"arguments", "args", "parameters", "input", "functionArguments"} {
				if z, ok := x[k]; ok {
					a = z
					break
				}
			}
			if a != nil {
				*out = append(*out, nativeToolCandidate{name: name, args: a})
				return
			}
		}
		for _, y := range x {
			walkNative(y, allowed, out)
		}
	}
}
