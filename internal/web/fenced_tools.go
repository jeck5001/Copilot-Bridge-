package web

import (
	"encoding/json"
	"regexp"
	"strings"
)

var fencedToolCall = regexp.MustCompile("(?s)```([A-Za-z0-9_-]+)\\s*\\n(.*?)\\n```")

func fencedToolCalls(text string, tools []map[string]any, choice any) []detectedToolCall {
	allowed := allowedToolNames(tools)
	var out []detectedToolCall
	for _, m := range fencedToolCall.FindAllStringSubmatch(text, -1) {
		name := m[1]
		if !allowed[name] || !toolChoiceAllows(choice, name) {
			continue
		}
		args := strings.TrimSpace(m[2])
		var v map[string]any
		if json.Unmarshal([]byte(args), &v) != nil {
			continue
		}
		call, ok := validatedFallbackToolCall(name, v, tools, choice, len(out))
		if !ok {
			continue
		}
		out = appendUniqueToolCall(out, call)
	}
	return out
}
