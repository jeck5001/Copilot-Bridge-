package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// Runtime settings permit up to 512 tool rounds. A normal sequential round
	// occupies an assistant call plus its tool result, so retain enough message
	// slots for the user goal and all 512 rounds. The byte limit remains a hard
	// safety boundary for unusually large/parallel results.
	maxRecoverableToolRounds = 512
	maxPortableMessages      = 1 + (maxRecoverableToolRounds * 3)
	// Eight MiB also guarantees that one maximum-width 64-call parallel round
	// can be recovered even when every bounded result reaches 64 KiB.
	maxPortableHistoryBytes = 8 << 20
	maxPortableContentBytes = 64 << 10
	// Runtime settings allow up to 64 parallel calls in one assistant turn.
	// Persist every call the client was told to execute; otherwise a later
	// function_call_output for call 9+ becomes an orphan after restoration.
	maxPortableToolCalls    = 64
	maxPortableToolArgBytes = 16 << 10
)

// compactPortableUTF8 bounds a persisted value by bytes without cutting a
// multi-byte rune. compactToolResult predates persisted portable history and
// slices raw bytes; using it here could turn boundary Chinese text into U+FFFD
// after the next json.Marshal.
func compactPortableUTF8(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit < 1 || len(value) <= limit {
		return value
	}
	prefix := func(text string, n int) string {
		if n >= len(text) {
			return text
		}
		text = text[:n]
		for len(text) > 0 && !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		return text
	}
	suffix := func(text string, n int) string {
		if n >= len(text) {
			return text
		}
		start := len(text) - n
		for start < len(text) && !utf8.RuneStart(text[start]) {
			start++
		}
		return text[start:]
	}
	// Reserve a stable maximum-width marker, then recompute the exact omitted
	// byte count. A final prefix guard keeps the result within limit even when
	// the decimal count grows.
	const markerReserve = 64
	if limit <= markerReserve {
		return prefix(value, limit)
	}
	headBudget := (limit - markerReserve) / 3
	tailBudget := limit - markerReserve - headBudget
	head := prefix(value, headBudget)
	tail := suffix(value, tailBudget)
	marker := fmt.Sprintf("\n... [truncated %d bytes] ...\n", len(value)-len(head)-len(tail))
	for len(head)+len(marker)+len(tail) > limit && len(tail) > 0 {
		tail = suffix(tail, len(tail)-1)
	}
	return head + marker + tail
}

func clonePortableMessages(messages []oaiMsg) []oaiMsg {
	if len(messages) == 0 {
		return nil
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	var cloned []oaiMsg
	if json.Unmarshal(raw, &cloned) != nil {
		return nil
	}
	return cloned
}

func sanitizePortableMessage(message oaiMsg) (oaiMsg, bool) {
	role := strings.ToLower(strings.TrimSpace(message.Role))
	switch role {
	case "system", "developer", "user", "assistant", "tool":
	default:
		return oaiMsg{}, false
	}
	clean := oaiMsg{Role: role, Name: compactPortableUTF8(message.Name, 256)}
	if role == "tool" {
		clean.ToolCallID = compactPortableUTF8(message.ToolCallID, 256)
	}
	if message.Content != nil {
		if text := strings.TrimSpace(contentToString(message.Content)); text != "" {
			clean.Content = compactPortableUTF8(text, maxPortableContentBytes)
		}
	}
	if role == "assistant" && len(message.ToolCalls) > 0 {
		limit := len(message.ToolCalls)
		if limit > maxPortableToolCalls {
			limit = maxPortableToolCalls
		}
		clean.ToolCalls = make([]map[string]any, 0, limit)
		for _, rawCall := range message.ToolCalls[:limit] {
			id, _ := rawCall["id"].(string)
			function, _ := rawCall["function"].(map[string]any)
			name, _ := function["name"].(string)
			arguments, ok := function["arguments"].(string)
			if !ok {
				arguments = mustJSON(function["arguments"])
			}
			id = compactPortableUTF8(id, 256)
			name = compactPortableUTF8(name, 256)
			arguments = compactPortableUTF8(arguments, maxPortableToolArgBytes)
			if id == "" || name == "" {
				continue
			}
			clean.ToolCalls = append(clean.ToolCalls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": arguments},
			})
		}
	}
	if clean.Content == nil && len(clean.ToolCalls) == 0 && role != "tool" {
		return oaiMsg{}, false
	}
	return clean, true
}

func portableMessagesBytes(messages []oaiMsg) int {
	raw, _ := json.Marshal(messages)
	return len(raw)
}

// boundedPortableMessages keeps a recent, protocol-ordered tail. Prefixes are
// dropped only at user-turn boundaries whenever possible so an assistant tool
// call and its tool result are never separated merely to satisfy a byte cap.
func boundedPortableMessages(messages []oaiMsg) []oaiMsg {
	clean := make([]oaiMsg, 0, len(messages))
	for _, message := range messages {
		if sanitized, ok := sanitizePortableMessage(message); ok {
			clean = append(clean, sanitized)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	if len(clean) <= maxPortableMessages && portableMessagesBytes(clean) <= maxPortableHistoryBytes {
		return clonePortableMessages(clean)
	}
	// A single long-running user turn can contain dozens of assistant/tool
	// pairs. Keep the user goal as an anchor and add the newest complete units;
	// blindly slicing the tail would create an orphan tool result and make the
	// failover transcript invalid exactly during long agent tasks.
	lastUser := -1
	for i := range clean {
		if clean[i].Role == "user" {
			lastUser = i
		}
	}
	if lastUser >= 0 {
		anchor := []oaiMsg{clean[lastUser]}
		var units [][]oaiMsg
		for i := lastUser + 1; i < len(clean); {
			end := i + 1
			if clean[i].Role == "assistant" && len(clean[i].ToolCalls) > 0 {
				for end < len(clean) && clean[end].Role == "tool" {
					end++
				}
			}
			units = append(units, clean[i:end])
			i = end
		}
		selected := []oaiMsg{}
		for i := len(units) - 1; i >= 0; i-- {
			candidateTail := append(clonePortableMessages(units[i]), selected...)
			candidate := append(clonePortableMessages(anchor), candidateTail...)
			if len(candidate) > maxPortableMessages || portableMessagesBytes(candidate) > maxPortableHistoryBytes {
				// Units are causally ordered. Skipping an oversized recent unit and
				// then admitting an older smaller one creates a hole in the restored
				// tool transcript, so stop at the first unit that does not fit.
				break
			}
			selected = candidateTail
		}
		return clonePortableMessages(append(anchor, selected...))
	}
	for len(clean) > maxPortableMessages || portableMessagesBytes(clean) > maxPortableHistoryBytes {
		if len(clean) == 1 {
			return clean
		}
		nextTurn := -1
		for i := 1; i < len(clean); i++ {
			if clean[i].Role == "user" {
				nextTurn = i
				break
			}
		}
		if nextTurn > 0 {
			clean = clean[nextTurn:]
		} else {
			clean = clean[1:]
		}
	}
	return clonePortableMessages(clean)
}

func portableMessageEqual(a, b oaiMsg) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

// portableTransition represents target exactly as parent[drop:]+delta. It is
// also able to encode a bounded sliding window: when old prefix messages fall
// out, drop advances instead of forcing every response alias to copy the whole
// remaining transcript.
func portableTransition(parent, target []oaiMsg) (drop int, delta []oaiMsg) {
	parent = boundedPortableMessages(parent)
	target = boundedPortableMessages(target)
	maxOverlap := len(parent)
	if len(target) < maxOverlap {
		maxOverlap = len(target)
	}
	overlap := 0
	for size := maxOverlap; size > 0; size-- {
		matched := true
		for i := 0; i < size; i++ {
			if !portableMessageEqual(parent[len(parent)-size+i], target[i]) {
				matched = false
				break
			}
		}
		if matched {
			overlap = size
			break
		}
	}
	return len(parent) - overlap, clonePortableMessages(target[overlap:])
}

// mergePortableMessages handles both Responses client styles: sending only the
// new item with previous_response_id, or resending a tail/full transcript.
func mergePortableMessages(history, incoming []oaiMsg) []oaiMsg {
	history = boundedPortableMessages(history)
	incoming = boundedPortableMessages(incoming)
	maxOverlap := len(history)
	if len(incoming) < maxOverlap {
		maxOverlap = len(incoming)
	}
	overlap := 0
	for size := maxOverlap; size > 0; size-- {
		matched := true
		for i := 0; i < size; i++ {
			if !portableMessageEqual(history[len(history)-size+i], incoming[i]) {
				matched = false
				break
			}
		}
		if matched {
			overlap = size
			break
		}
	}
	merged := append(clonePortableMessages(history), incoming[overlap:]...)
	return boundedPortableMessages(merged)
}

func assistantPortableMessage(text string, calls []detectedToolCall) oaiMsg {
	message := oaiMsg{Role: "assistant"}
	if len(calls) == 0 {
		message.Content = text
		return message
	}
	for _, raw := range toolCallMaps(calls) {
		if call, ok := raw.(map[string]any); ok {
			message.ToolCalls = append(message.ToolCalls, call)
		}
	}
	return message
}

func assistantPortableMessageFromOpenAI(source map[string]any) (oaiMsg, bool) {
	message, _ := openAIChoice(source)
	if message == nil {
		return oaiMsg{}, false
	}
	portable := oaiMsg{Role: "assistant", Content: message["content"]}
	if rawCalls, ok := message["tool_calls"].([]any); ok {
		for _, raw := range rawCalls {
			if call, ok := raw.(map[string]any); ok {
				portable.ToolCalls = append(portable.ToolCalls, call)
			}
		}
	}
	portable, ok := sanitizePortableMessage(portable)
	return portable, ok
}
