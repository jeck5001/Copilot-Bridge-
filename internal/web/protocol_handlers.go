package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/google/uuid"
)

type pipeResponseWriter struct {
	h      http.Header
	w      *io.PipeWriter
	status int
}

// Responses clients consume typed SSE events, not comment frames. In
// particular, Hermes refreshes its stream watchdog only after the OpenAI SDK
// has parsed an event, while OpenCode uses the Chat Completions endpoint and
// relies on the transport-level comment heartbeat. Keep the generic comment
// heartbeat for Chat Completions and emit a valid Responses lifecycle event
// while this adapter is waiting for ChatHub to finish reasoning.
var responsesProgressHeartbeatInterval = 5 * time.Second
var responsesProducerShutdownTimeout = 5 * time.Second

func responsesResourceFields(body responsesRequest) map[string]any {
	parallel := true
	if body.ParallelToolCalls != nil {
		parallel = *body.ParallelToolCalls
	}
	toolChoice := body.ToolChoice
	if toolChoice == nil {
		toolChoice = "auto"
	}
	metadata := body.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	tools := body.Tools
	if tools == nil {
		tools = []map[string]any{}
	}
	store := true
	if body.Store != nil {
		store = *body.Store
	}
	fields := map[string]any{
		"instructions": body.Instructions, "metadata": metadata,
		"max_output_tokens": body.MaxOutputTokens, "parallel_tool_calls": parallel,
		"previous_response_id": nil, "reasoning": body.Reasoning, "store": store,
		"temperature": 1.0, "text": map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice": toolChoice, "tools": tools, "top_p": 1.0, "truncation": "disabled",
	}
	if body.PreviousResponseID != "" {
		fields["previous_response_id"] = body.PreviousResponseID
	}
	if body.Conversation != nil {
		fields["conversation"] = body.Conversation
	}
	if body.PromptCacheKey != "" {
		fields["prompt_cache_key"] = body.PromptCacheKey
	}
	if body.ServiceTier != "" {
		fields["service_tier"] = body.ServiceTier
	}
	return fields
}

func applyResponsesResourceFields(response map[string]any, fields map[string]any) {
	if fields == nil {
		fields = responsesResourceFields(responsesRequest{})
	}
	for key, value := range fields {
		response[key] = value
	}
}

func (p *pipeResponseWriter) Header() http.Header { return p.h }
func (p *pipeResponseWriter) WriteHeader(n int) {
	if p.status == 0 {
		p.status = n
	}
}
func (p *pipeResponseWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = 200
	}
	return p.w.Write(b)
}
func (p *pipeResponseWriter) Flush() {}

// streamResponsesAdapter converts the internal OpenAI SSE incrementally instead
// of buffering the entire completion in httptest.ResponseRecorder.
func (s *Server) streamResponsesAdapter(w http.ResponseWriter, r *http.Request, o oaiReq, model string, previousResponse bool, resourceFields map[string]any) {
	o.Stream = true
	responseID := "resp_" + uuid.NewString()
	targetKey := responseIDSessionKey(responseID)
	if o.SessionKey == "" {
		o.SessionKey = targetKey
	}
	discardTarget, err := s.prepareResponseTarget(r, o.SessionKey, targetKey, previousResponse)
	if err != nil {
		writeResponsesError(w, http.StatusConflict, "previous_response_unavailable", err.Error())
		return
	}
	committed := false
	defer func() {
		discardTarget(committed)
	}()
	o.SessionWriteKey = targetKey
	b, _ := json.Marshal(o)
	streamCtx, cancel := context.WithCancel(r.Context())
	r2 := r.Clone(streamCtx)
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	pr, pw := io.Pipe()
	irw := &pipeResponseWriter{h: make(http.Header), w: pw}
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		s.openaiChat(irw, r2)
		_ = pw.Close()
	}()
	defer func() {
		cancel()
		_ = pr.CloseWithError(context.Canceled)
		timer := time.NewTimer(responsesProducerShutdownTimeout)
		defer timer.Stop()
		select {
		case <-producerDone:
		case <-timer.C:
			log.Printf("[responses] producer did not stop within %s after cancellation", responsesProducerShutdownTimeout)
		}
	}()

	_ = streamResponsesFromReaderIDFields(w, pr, model, responseID, resourceFields, func(assistant oaiMsg) error {
		if err := s.commitResponsePortableHistory(r, o.SessionKey, targetKey, o.Messages, assistant, !previousResponse); err != nil {
			return fmt.Errorf("persist streamed portable history: %w", err)
		}
		committed = true
		return nil
	})
}

func streamResponsesFromReader(w http.ResponseWriter, reader io.Reader, model string) error {
	return streamResponsesFromReaderID(w, reader, model, "resp_"+uuid.NewString(), nil)
}

func streamResponsesFromReaderID(w http.ResponseWriter, reader io.Reader, model, id string, onCompleted func(oaiMsg) error) error {
	return streamResponsesFromReaderIDFields(w, reader, model, id, nil, onCompleted)
}

func streamResponsesFromReaderIDFields(w http.ResponseWriter, reader io.Reader, model, id string, resourceFields map[string]any, onCompleted func(oaiMsg) error) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	sequence := int64(0)
	lastResponseEvent := time.Now()
	emit := func(name string, v any) error {
		if event, ok := v.(map[string]any); ok {
			event["sequence_number"] = sequence
		}
		sequence++
		if err := writeSSE(w, name, v); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		lastResponseEvent = time.Now()
		return nil
	}
	created := time.Now().Unix()
	responseBase := func(status string, output []any) map[string]any {
		response := map[string]any{
			"id": id, "object": "response", "created_at": created,
			"status": status, "model": model, "output": output,
			"error": nil, "incomplete_details": nil, "usage": nil,
		}
		applyResponsesResourceFields(response, resourceFields)
		return response
	}
	if err := emit("response.created", map[string]any{"type": "response.created", "response": responseBase("in_progress", []any{})}); err != nil {
		return err
	}
	if err := emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": responseBase("in_progress", []any{})}); err != nil {
		return err
	}

	var text strings.Builder
	messageID := "msg_" + uuid.NewString()
	textStarted := false
	messageOutputIndex := -1
	nextOutputIndex := 0
	type tcState struct {
		ID, Name, Args string
		ItemID         string
		OutputIndex    int
		Added          bool
	}
	calls := map[int]*tcState{}
	callOrder := []*tcState{}
	var upstreamError strings.Builder
	var upstreamCode string
	var upstreamFinishReason string
	upstreamFailed := false
	sawDone := false
	type scanResult struct {
		line string
		err  error
		done bool
	}
	scanResults := make(chan scanResult, 1)
	stopScan := make(chan struct{})
	defer close(stopScan)
	go func() {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		for scanner.Scan() {
			select {
			case scanResults <- scanResult{line: scanner.Text()}:
			case <-stopScan:
				return
			}
		}
		select {
		case scanResults <- scanResult{err: scanner.Err(), done: true}:
		case <-stopScan:
		}
	}()

	var progressTicker *time.Ticker
	var progressTick <-chan time.Time
	if responsesProgressHeartbeatInterval > 0 {
		progressTicker = time.NewTicker(responsesProgressHeartbeatInterval)
		progressTick = progressTicker.C
		defer progressTicker.Stop()
	}
	var scanErr error
scanLoop:
	for {
		var line string
		select {
		case result := <-scanResults:
			if result.done {
				scanErr = result.err
				break scanLoop
			}
			line = result.line
		case <-progressTick:
			if time.Since(lastResponseEvent) < responsesProgressHeartbeatInterval {
				continue
			}
			// A repeated in_progress event is understood by the OpenAI SDK and
			// is deliberately content-free: it proves liveness without
			// manufacturing output or disturbing item indexes.
			if err := emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": responseBase("in_progress", []any{})}); err != nil {
				return err
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// Non-SSE content from the upstream handler is usually an error
			// payload written by http.Error. Preserve it so clients receive a
			// real error instead of an unexplained stream disconnect.
			if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "event:") && !strings.HasPrefix(trimmed, ":") {
				var envelope map[string]any
				if json.Unmarshal([]byte(trimmed), &envelope) == nil {
					if detail, ok := envelope["error"].(map[string]any); ok {
						if message, ok := detail["message"].(string); ok && message != "" {
							upstreamError.WriteString(message)
							upstreamError.WriteString("; ")
						}
						if code, ok := detail["type"].(string); ok && code != "" {
							upstreamCode = code
						}
						upstreamFailed = true
						continue
					}
				}
				upstreamError.WriteString(trimmed)
				upstreamError.WriteString("; ")
			}
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			upstreamFailed = true
			upstreamError.WriteString("malformed upstream SSE data; ")
			continue
		}
		if em, ok := chunk["error"].(map[string]any); ok {
			if emsg, ok := em["message"].(string); ok && emsg != "" {
				upstreamError.WriteString(emsg)
				upstreamError.WriteString("; ")
			}
			if code, ok := em["code"].(string); ok && code != "" {
				upstreamCode = code
			}
			upstreamFailed = true
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
			upstreamFinishReason = finish
		}
		delta, _ := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok && content != "" {
			text.WriteString(content)
			if !textStarted {
				textStarted = true
				messageOutputIndex = nextOutputIndex
				nextOutputIndex++
				if err := emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": messageOutputIndex, "item": map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{}}}); err != nil {
					return err
				}
				if err := emit("response.content_part.added", map[string]any{"type": "response.content_part.added", "output_index": messageOutputIndex, "content_index": 0, "item_id": messageID, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
					return err
				}
			}
			if err := emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": messageOutputIndex, "content_index": 0, "item_id": messageID, "delta": content}); err != nil {
				return err
			}
		}
		if rawCalls, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range rawCalls {
				tc, _ := raw.(map[string]any)
				idxf, _ := tc["index"].(float64)
				idx := int(idxf)
				st := calls[idx]
				if st == nil {
					st = &tcState{ItemID: "fc_" + uuid.NewString(), OutputIndex: nextOutputIndex}
					nextOutputIndex++
					calls[idx] = st
					callOrder = append(callOrder, st)
				}
				if v, ok := tc["id"].(string); ok {
					st.ID = v
				}
				fn, _ := tc["function"].(map[string]any)
				if v, ok := fn["name"].(string); ok {
					st.Name += v
				}
				if !st.Added {
					st.Added = true
					if err := emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": st.OutputIndex, "item": map[string]any{"type": "function_call", "id": st.ItemID, "call_id": st.ID, "name": st.Name, "arguments": "", "status": "in_progress"}}); err != nil {
						return err
					}
				}
				if v, ok := fn["arguments"].(string); ok {
					st.Args += v
					if err := emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": st.OutputIndex, "item_id": st.ItemID, "delta": v}); err != nil {
						return err
					}
				}
			}
		}
	}
	if scanErr != nil {
		upstreamFailed = true
		upstreamError.WriteString("upstream stream read failed: " + scanErr.Error() + "; ")
	}
	if !sawDone {
		upstreamFailed = true
		upstreamError.WriteString("upstream stream disconnected before [DONE]; ")
	}
	if upstreamFailed || (upstreamFinishReason != "length" && len(calls) == 0 && strings.TrimSpace(text.String()) == "") {
		// The upstream connection can close normally without producing a
		// response. Do not emit a completed Responses resource with an empty
		// message ID that clients may try to reference on the next turn.
		// Surface the upstream failure explicitly instead of closing silently,
		// which clients would otherwise report as an unexplained disconnect.
		msg := strings.TrimSpace(upstreamError.String())
		if msg == "" {
			msg = "upstream returned no content"
		}
		code := "upstream_error"
		if upstreamCode != "" {
			code = upstreamCode
		}
		failure := map[string]any{"code": code, "message": msg}
		failedResponse := responseBase("failed", []any{})
		failedResponse["error"] = failure
		if err := emit("response.failed", map[string]any{"type": "response.failed", "response": failedResponse}); err != nil {
			return err
		}
		return nil
	}
	output := make([]any, nextOutputIndex)
	itemStatus := "completed"
	responseStatus := "completed"
	terminalEvent := "response.completed"
	if upstreamFinishReason == "length" {
		itemStatus = "incomplete"
		responseStatus = "incomplete"
		terminalEvent = "response.incomplete"
	}
	var textPart map[string]any
	if textStarted {
		textPart = map[string]any{"type": "output_text", "text": text.String(), "annotations": []any{}}
		item := map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": itemStatus, "content": []any{textPart}}
		output[messageOutputIndex] = item
	}
	for _, st := range callOrder {
		item := map[string]any{"type": "function_call", "id": st.ItemID, "call_id": st.ID, "name": st.Name, "arguments": st.Args, "status": itemStatus}
		output[st.OutputIndex] = item
	}
	resp := responseBase(responseStatus, output)
	if responseStatus == "incomplete" {
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if onCompleted != nil {
		assistant := oaiMsg{Role: "assistant"}
		if textStarted {
			assistant.Content = text.String()
		}
		// Truncated function arguments are not executable calls. Persisting them
		// would make the next previous_response_id turn wait for a tool result the
		// client was never told to execute.
		if responseStatus == "completed" {
			for _, state := range callOrder {
				assistant.ToolCalls = append(assistant.ToolCalls, map[string]any{
					"id": state.ID, "type": "function",
					"function": map[string]any{"name": state.Name, "arguments": state.Args},
				})
			}
		}
		commitDone := make(chan error, 1)
		go func() { commitDone <- onCompleted(assistant) }()
		var commitErr error
	commitLoop:
		for {
			select {
			case commitErr = <-commitDone:
				break commitLoop
			case <-progressTick:
				if responsesProgressHeartbeatInterval > 0 && time.Since(lastResponseEvent) >= responsesProgressHeartbeatInterval {
					if err := emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": responseBase("in_progress", []any{})}); err != nil {
						// The commit cannot be cancelled safely once atomic persistence
						// starts. Keep waiting for commitDone so cleanup cannot race a
						// successful commit after the client disconnects.
						commitErr = err
						if callbackErr := <-commitDone; callbackErr != nil {
							return callbackErr
						}
						return commitErr
					}
				}
			}
		}
		if commitErr != nil {
			log.Printf("[sessions] response %s cannot be continued: %v", id, commitErr)
			// No output item is executable until persistence succeeds. A failed
			// response therefore must not expose status=completed tool calls.
			failedResponse := responseBase("failed", []any{})
			failedResponse["error"] = map[string]any{"code": "session_persistence_error", "message": "response state could not be persisted for continuation"}
			if emitErr := emit("response.failed", map[string]any{"type": "response.failed", "response": failedResponse}); emitErr != nil {
				return emitErr
			}
			return nil
		}
	}
	// A client may execute a function as soon as it sees an arguments.done or
	// output_item.done event. Persist the continuation state first so a disk
	// failure cannot expose an executable call whose previous_response_id does
	// not exist, forcing a retry that repeats the side effect.
	if textStarted {
		if err := emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": messageOutputIndex, "content_index": 0, "item_id": messageID, "text": text.String()}); err != nil {
			return err
		}
		if err := emit("response.content_part.done", map[string]any{"type": "response.content_part.done", "output_index": messageOutputIndex, "content_index": 0, "item_id": messageID, "part": textPart}); err != nil {
			return err
		}
		if err := emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": messageOutputIndex, "item": output[messageOutputIndex]}); err != nil {
			return err
		}
	}
	// A length-limited function call may contain truncated JSON arguments. Do
	// not emit *.done for it; response.incomplete is the only terminal signal.
	if responseStatus == "completed" {
		for _, st := range callOrder {
			if err := emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": st.OutputIndex, "item_id": st.ItemID, "name": st.Name, "arguments": st.Args}); err != nil {
				return err
			}
			if err := emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": st.OutputIndex, "item": output[st.OutputIndex]}); err != nil {
				return err
			}
		}
		resp["completed_at"] = time.Now().Unix()
	}
	if err := emit(terminalEvent, map[string]any{"type": terminalEvent, "response": resp}); err != nil {
		return err
	}
	return nil
}

func (s *Server) runOpenAIAdapter(r *http.Request, o oaiReq) (map[string]any, []byte, int, error) {
	o.Stream = false
	b, _ := json.Marshal(o)
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	rr := httptest.NewRecorder()
	s.openaiChat(rr, r2)
	var out map[string]any
	err := json.Unmarshal(rr.Body.Bytes(), &out)
	return out, rr.Body.Bytes(), rr.Code, err
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeResponsesError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body responsesRequest
	payload, readErr := io.ReadAll(r.Body)
	if readErr != nil || json.Unmarshal(payload, &body) != nil {
		writeResponsesError(w, 400, "invalid_request_error", "bad json")
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || fields == nil {
		writeResponsesError(w, 400, "invalid_request_error", "request body must be a JSON object")
		return
	}
	if err := body.validateSemantics(fields); err != nil {
		writeResponsesError(w, 400, "unsupported_parameter", err.Error())
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeResponsesError(w, 400, "invalid_request_error", err.Error())
		return
	}
	if body.Stream {
		// The streaming adapter owns its response ID because it must emit it
		// before the internal Chat Completions producer yields content.
		s.streamResponsesAdapter(w, r, o, firstNonEmpty(body.Model, "m365-copilot"), strings.TrimSpace(body.PreviousResponseID) != "", responsesResourceFields(body))
		return
	}
	responseID := "resp_" + uuid.NewString()
	targetKey := responseIDSessionKey(responseID)
	if o.SessionKey == "" {
		o.SessionKey = targetKey
	}
	previousResponse := strings.TrimSpace(body.PreviousResponseID) != ""
	discardTarget, err := s.prepareResponseTarget(r, o.SessionKey, targetKey, previousResponse)
	if err != nil {
		writeResponsesError(w, http.StatusConflict, "previous_response_unavailable", err.Error())
		return
	}
	committed := false
	defer func() {
		discardTarget(committed)
	}()
	o.SessionWriteKey = targetKey
	out, raw, status, err := s.runOpenAIAdapter(r, o)
	if status >= 400 {
		writeResponsesError(w, status, errorType(raw, "upstream_error"), errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "upstream protocol error: "+err.Error())
		return
	}
	_, finishReason := openAIChoice(out)
	if finishReason != "length" && !responsesOutputHasContent(out) {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "ChatHub returned an empty response; no reusable message was created")
		return
	}
	assistant, hasAssistant := assistantPortableMessageFromOpenAI(out)
	if !hasAssistant && finishReason == "length" {
		assistant = oaiMsg{Role: "assistant", Content: ""}
		hasAssistant = true
	}
	if hasAssistant {
		if err := s.commitResponsePortableHistory(r, o.SessionKey, targetKey, o.Messages, assistant, !previousResponse); err != nil {
			log.Printf("[sessions] commit response %s: %v", responseID, err)
			writeResponsesError(w, http.StatusInternalServerError, "session_persistence_error", "response state could not be persisted for continuation")
			return
		}
		committed = true
	}
	writeResponsesResultWithIDFields(w, responseID, firstNonEmpty(body.Model, "m365-copilot"), body.Stream, out, responsesResourceFields(body))
}

func responsesOutputHasContent(src map[string]any) bool {
	msg, _ := openAIChoice(src)
	if msg == nil {
		return false
	}
	if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	text, _ := msg["content"].(string)
	return strings.TrimSpace(text) != ""
}

func (s *Server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body anthropicRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeAnthropicError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	out, raw, status, err := s.runOpenAIAdapter(r, o)
	if status >= 400 {
		writeAnthropicError(w, status, "api_error", errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream protocol error: "+err.Error())
		return
	}
	writeAnthropicResult(w, firstNonEmpty(body.Model, "m365-copilot"), body.Stream, out)
}
