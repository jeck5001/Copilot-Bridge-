package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestResponsesUnknownPreviousToolOutputRejectedWithoutUpstream(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(string, int) gatewayHubOutcome {
		return gatewayHubSuccess
	}, "account-1", "account-2")
	body := map[string]any{
		"model":                "gpt-5.6-sol",
		"previous_response_id": "resp_does_not_exist",
		"input": []any{map[string]any{
			"type": "function_call_output", "call_id": "call_unknown", "output": "must not reach upstream",
		}},
	}
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer unknown-alias-test")
	response := httptest.NewRecorder()
	server.responses(response, request)
	if response.Code != http.StatusConflict || !bytes.Contains(response.Body.Bytes(), []byte("previous_response_unavailable")) {
		t.Fatalf("status=%d body=%s; want previous_response_unavailable", response.Code, response.Body.String())
	}
	if calls, _ := hub.snapshot(); len(calls) != 0 {
		t.Fatalf("unknown previous_response_id reached upstream: %v", calls)
	}
}

func TestResponsesWrongPreviousCallIDRejectedWithoutUpstream(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(string, int) gatewayHubOutcome {
		return gatewayHubSuccess
	}, "account-1", "account-2")
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer wrong-call-test")
	previousID := "resp_known"
	source := scopedSessionKey(request, responseIDSessionKey(previousID))
	if _, err := server.sessions.upsert(conversation{
		ID: source, AccountID: "account-1", ConversationID: "conversation-1", SessionID: "session-1",
		PortableMessages: []oaiMsg{
			{Role: "user", Content: "call inspect"},
			{Role: "assistant", ToolCalls: []map[string]any{{
				"id": "call_expected", "type": "function",
				"function": map[string]any{"name": "inspect", "arguments": `{}`},
			}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"model":                "gpt-5.6-sol",
		"previous_response_id": previousID,
		"input": []any{map[string]any{
			"type": "function_call_output", "call_id": "call_wrong", "output": "must not reach upstream",
		}},
	}
	encoded, _ := json.Marshal(body)
	request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer wrong-call-test")
	response := httptest.NewRecorder()
	server.responses(response, request)
	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte("tool_protocol_error")) {
		t.Fatalf("status=%d body=%s; want tool_protocol_error", response.Code, response.Body.String())
	}
	if calls, _ := hub.snapshot(); len(calls) != 0 {
		t.Fatalf("wrong call_id reached upstream: %v", calls)
	}
}

func TestConcurrentIdleResponsesStayLiveAndComplete(t *testing.T) {
	oldInterval := responsesProgressHeartbeatInterval
	responsesProgressHeartbeatInterval = 2 * time.Millisecond
	defer func() { responsesProgressHeartbeatInterval = oldInterval }()

	const workers = 64
	errors := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			reader, writer := io.Pipe()
			recorder := httptest.NewRecorder()
			done := make(chan error, 1)
			go func() { done <- streamResponsesFromReader(recorder, reader, "gpt-5.6-sol") }()
			time.Sleep(15 * time.Millisecond)
			_, _ = fmt.Fprintf(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"worker-%d\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", worker)
			_ = writer.Close()
			if err := <-done; err != nil {
				errors <- fmt.Errorf("worker %d: %w", worker, err)
				return
			}
			events := responseStreamEvents(t, recorder.Body.String())
			progress := 0
			completed := false
			for index, event := range events {
				if got, _ := event["sequence_number"].(float64); int(got) != index {
					errors <- fmt.Errorf("worker %d sequence %d=%v", worker, index, event["sequence_number"])
					return
				}
				switch event["type"] {
				case "response.in_progress":
					progress++
				case "response.completed":
					completed = true
				case "error", "response.failed":
					errors <- fmt.Errorf("worker %d terminal failure: %v", worker, event)
					return
				}
			}
			// response.in_progress is emitted once at creation and then by the
			// heartbeat ticker. Under -race with 64 runnable workers, ticker events
			// may be coalesced by the scheduler; require proof of at least one real
			// heartbeat rather than a wall-clock-dependent exact count.
			if progress < 2 || !completed {
				errors <- fmt.Errorf("worker %d progress=%d completed=%t", worker, progress, completed)
			}
		}(worker)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestToolProtocolLongSerialAndParallelMatrix(t *testing.T) {
	messages := []oaiMsg{{Role: "user", Content: "start"}}
	for round := 0; round < 128; round++ {
		first := fmt.Sprintf("call_%03d_a", round)
		second := fmt.Sprintf("call_%03d_b", round)
		messages = append(messages,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{
				{"id": first, "type": "function", "function": map[string]any{"name": "inspect", "arguments": `{}`}},
				{"id": second, "type": "function", "function": map[string]any{"name": "inspect", "arguments": `{}`}},
			}},
			// Parallel tool outputs are allowed to arrive in either order.
			oaiMsg{Role: "tool", ToolCallID: second, Content: "B"},
			oaiMsg{Role: "tool", ToolCallID: first, Content: "A"},
		)
	}
	if err := validateToolConversation(messages); err != nil {
		t.Fatalf("128-round mixed serial/parallel history invalid: %v", err)
	}

	store := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	bounded := boundedPortableMessages(messages)
	if err := validateToolConversation(bounded); err != nil {
		t.Fatalf("bounded long tool history split a call/result group: %v", err)
	}
	if _, err := store.upsert(conversation{ID: "long-tools", PortableMessages: bounded}); err != nil {
		t.Fatal(err)
	}
}
