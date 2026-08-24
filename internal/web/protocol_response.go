package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

func openAIChoice(v map[string]any) (map[string]any, string) {
	choices, _ := v["choices"].([]any)
	if len(choices) == 0 {
		return nil, ""
	}
	c, _ := choices[0].(map[string]any)
	m, _ := c["message"].(map[string]any)
	finish, _ := c["finish_reason"].(string)
	return m, finish
}

func writeResponsesResult(w http.ResponseWriter, model string, stream bool, src map[string]any) {
	writeResponsesResultWithID(w, "resp_"+uuid.NewString(), model, stream, src)
}

func writeResponsesResultWithID(w http.ResponseWriter, id, model string, stream bool, src map[string]any) {
	writeResponsesResultWithIDFields(w, id, model, stream, src, nil)
}

func writeResponsesResultWithIDFields(w http.ResponseWriter, id, model string, stream bool, src map[string]any, resourceFields map[string]any) {
	msg, finish := openAIChoice(src)
	responseStatus := "completed"
	itemStatus := "completed"
	terminalEvent := "response.completed"
	var incompleteDetails any
	if finish == "length" {
		responseStatus = "incomplete"
		itemStatus = "incomplete"
		terminalEvent = "response.incomplete"
		incompleteDetails = map[string]any{"reason": "max_output_tokens"}
	}
	var output []any
	if calls, ok := msg["tool_calls"].([]any); ok {
		if len(calls) > 0 {
			if summary := toolPlanSummaryFromMaps(calls); summary != "" {
				output = append(output, map[string]any{"type": "message", "id": "msg_" + uuid.NewString(), "role": "assistant", "status": itemStatus, "content": []any{map[string]any{"type": "output_text", "text": summary, "annotations": []any{}}}})
			}
		}
		for _, raw := range calls {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			output = append(output, map[string]any{"type": "function_call", "id": "fc_" + uuid.NewString(), "call_id": tc["id"], "name": fn["name"], "arguments": fn["arguments"], "status": itemStatus})
		}
	} else if msg != nil || finish != "length" {
		text, _ := msg["content"].(string)
		messageID := "msg_" + uuid.NewString()
		output = append(output, map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": itemStatus, "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}})
	}
	resp := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": responseStatus, "model": model, "output": output, "incomplete_details": incompleteDetails}
	applyResponsesResourceFields(resp, resourceFields)
	if responseStatus == "completed" {
		resp["completed_at"] = time.Now().Unix()
	}
	if !stream {
		jsonOut(w, resp)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(name string, v any) {
		_ = writeSSE(w, name, v)
	}
	createdResponse := map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model, "output": []any{}}
	applyResponsesResourceFields(createdResponse, resourceFields)
	emit("response.created", map[string]any{"type": "response.created", "response": createdResponse})
	for i, item := range output {
		m, _ := item.(map[string]any)
		addedItem := item
		if m["type"] == "function_call" {
			// Arguments are streamed by function_call_arguments.delta. Starting
			// with the completed arguments here makes conforming clients append
			// the same JSON twice and produces an invalid `{...}{...}` value.
			added := make(map[string]any, len(m))
			for k, v := range m {
				added[k] = v
			}
			added["arguments"] = ""
			added["status"] = "in_progress"
			addedItem = added
		}
		emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": i, "item": addedItem})
		if m["type"] == "message" {
			content, _ := m["content"].([]any)
			if len(content) > 0 {
				c, _ := content[0].(map[string]any)
				emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": i, "content_index": 0, "delta": c["text"]})
			}
		} else if m["type"] == "function_call" {
			args, _ := m["arguments"].(string)
			emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "item_id": m["id"], "delta": args})
			if responseStatus == "completed" {
				emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "item_id": m["id"], "arguments": args})
			}
		}
		if m["type"] != "function_call" || responseStatus == "completed" {
			emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
		}
	}
	emit(terminalEvent, map[string]any{"type": terminalEvent, "response": resp})
}

func writeAnthropicResult(w http.ResponseWriter, model string, stream bool, src map[string]any) {
	id := "msg_" + uuid.NewString()
	msg, finish := openAIChoice(src)
	blocks := []any{}
	stop := "end_turn"
	if calls, ok := msg["tool_calls"].([]any); ok {
		stop = "tool_use"
		for _, raw := range calls {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			var input any = map[string]any{}
			if a, ok := fn["arguments"].(string); ok {
				_ = json.Unmarshal([]byte(a), &input)
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc["id"], "name": fn["name"], "input": input})
		}
	} else {
		blocks = append(blocks, map[string]any{"type": "text", "text": fmt.Sprint(msg["content"])})
	}
	_ = finish
	out := map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}, "m365": map[string]any{"usage_source": "unavailable_from_chathub", "usage_values_are_placeholders": true}}
	if !stream {
		jsonOut(w, out)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(n string, v any) {
		_ = writeSSE(w, n, v)
	}
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
	for i, b := range blocks {
		m, _ := b.(map[string]any)
		startBlock := b
		if m["type"] == "tool_use" {
			startBlock = map[string]any{"type": "tool_use", "id": m["id"], "name": m["name"], "input": map[string]any{}}
		}
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": startBlock})
		if m["type"] == "text" {
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": m["text"]}})
		} else if m["type"] == "tool_use" {
			partial, _ := json.Marshal(m["input"])
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(partial)}})
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}})
	emit("message_stop", map[string]any{"type": "message_stop"})
}
