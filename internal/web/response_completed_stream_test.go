package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func responseStreamEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func TestResponsesAdapterRequiresDoneAndEmitsCanonicalLifecycle(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello "}}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"world"},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	rr := httptest.NewRecorder()
	if err := streamResponsesFromReader(rr, strings.NewReader(upstream), "test-model"); err != nil {
		t.Fatal(err)
	}
	events := responseStreamEvents(t, rr.Body.String())
	if len(events) == 0 {
		t.Fatal("no response events")
	}
	types := map[string]bool{}
	for i, event := range events {
		types[fmt.Sprint(event["type"])] = true
		if got, _ := event["sequence_number"].(float64); int(got) != i {
			t.Fatalf("event %d sequence_number=%v", i, event["sequence_number"])
		}
	}
	for _, required := range []string{"response.content_part.added", "response.output_text.done", "response.content_part.done", "response.function_call_arguments.done", "response.completed"} {
		if !types[required] {
			t.Fatalf("missing %s in %v", required, types)
		}
	}
	last := events[len(events)-1]
	response, _ := last["response"].(map[string]any)
	output, _ := response["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("completed output=%#v", output)
	}
	for _, event := range events {
		if event["type"] == "response.function_call_arguments.done" && event["name"] != "lookup" {
			t.Fatalf("function done name=%v", event["name"])
		}
	}
}

func TestResponsesAdapterDoesNotCompletePartialStream(t *testing.T) {
	upstream := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	rr := httptest.NewRecorder()
	if err := streamResponsesFromReader(rr, strings.NewReader(upstream), "test-model"); err != nil {
		t.Fatal(err)
	}
	events := responseStreamEvents(t, rr.Body.String())
	failedCount := 0
	for _, event := range events {
		switch event["type"] {
		case "error":
			t.Fatalf("partial stream emitted a duplicate error terminal: %#v", event)
		case "response.failed":
			failedCount++
			response, _ := event["response"].(map[string]any)
			failure, _ := response["error"].(map[string]any)
			if !strings.Contains(fmt.Sprint(failure["message"]), "[DONE]") {
				t.Fatalf("failed response lost the terminal cause: %#v", event)
			}
		case "response.completed":
			t.Fatalf("partial stream emitted completed: %#v", event)
		}
	}
	if failedCount != 1 || events[len(events)-1]["type"] != "response.failed" {
		t.Fatalf("missing partial-stream failure lifecycle: %s", rr.Body.String())
	}
}

func TestResponsesAdapterPreservesLengthAsIncomplete(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"partial but valid"},"finish_reason":"length"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	rr := httptest.NewRecorder()
	if err := streamResponsesFromReader(rr, strings.NewReader(upstream), "test-model"); err != nil {
		t.Fatal(err)
	}
	events := responseStreamEvents(t, rr.Body.String())
	foundIncomplete := false
	for _, event := range events {
		switch event["type"] {
		case "response.completed":
			t.Fatalf("length-limited stream emitted completed: %#v", event)
		case "response.incomplete":
			foundIncomplete = true
			response, _ := event["response"].(map[string]any)
			if response["status"] != "incomplete" {
				t.Fatalf("response status=%v", response["status"])
			}
			details, _ := response["incomplete_details"].(map[string]any)
			if details["reason"] != "max_output_tokens" {
				t.Fatalf("incomplete details=%#v", details)
			}
		}
	}
	if !foundIncomplete {
		t.Fatalf("missing response.incomplete: %s", rr.Body.String())
	}
}

func TestResponsesAdapterLengthWithoutVisibleOutputIsIncomplete(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	rr := httptest.NewRecorder()
	if err := streamResponsesFromReader(rr, strings.NewReader(upstream), "test-model"); err != nil {
		t.Fatal(err)
	}
	events := responseStreamEvents(t, rr.Body.String())
	for _, event := range events {
		if event["type"] == "response.failed" || event["type"] == "response.completed" {
			t.Fatalf("empty length response used wrong terminal event: %#v", event)
		}
		if event["type"] == "response.incomplete" {
			response, _ := event["response"].(map[string]any)
			if _, exists := response["completed_at"]; exists {
				t.Fatalf("incomplete response exposed completed_at: %#v", response)
			}
			return
		}
	}
	t.Fatalf("missing response.incomplete: %s", rr.Body.String())
}

func TestResponsesPersistenceFailurePrecedesAllDoneEvents(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_side_effect","function":{"name":"write_file","arguments":"{\"path\":\"a\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	rr := httptest.NewRecorder()
	err := streamResponsesFromReaderID(rr, strings.NewReader(upstream), "test-model", "resp_test", func(oaiMsg) error {
		return fmt.Errorf("disk full")
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range responseStreamEvents(t, rr.Body.String()) {
		typ, _ := event["type"].(string)
		switch typ {
		case "response.function_call_arguments.done", "response.output_item.done", "response.completed", "response.incomplete":
			t.Fatalf("persistence failure exposed an executable/completed item: %#v", event)
		}
	}
	if !strings.Contains(rr.Body.String(), `"type":"response.failed"`) {
		t.Fatalf("missing response.failed: %s", rr.Body.String())
	}
}

func TestResponsesAdapterEmitsTypedProgressWhileUpstreamIsIdle(t *testing.T) {
	oldInterval := responsesProgressHeartbeatInterval
	responsesProgressHeartbeatInterval = 10 * time.Millisecond
	defer func() { responsesProgressHeartbeatInterval = oldInterval }()

	upstreamReader, upstreamWriter := io.Pipe()
	rr := httptest.NewRecorder()
	done := make(chan error, 1)
	go func() {
		done <- streamResponsesFromReader(rr, upstreamReader, "test-model")
	}()

	// Keep ChatHub silent for several heartbeat intervals, then finish with a
	// normal response. This is the state that previously made Hermes retry at
	// 12/60 seconds even though transport comment heartbeats were flowing.
	time.Sleep(35 * time.Millisecond)
	_, _ = io.WriteString(upstreamWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	_ = upstreamWriter.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	events := responseStreamEvents(t, rr.Body.String())
	inProgress := 0
	completed := false
	for i, event := range events {
		if got, _ := event["sequence_number"].(float64); int(got) != i {
			t.Fatalf("event %d sequence_number=%v", i, event["sequence_number"])
		}
		switch event["type"] {
		case "response.in_progress":
			inProgress++
		case "response.completed":
			completed = true
		}
	}
	if inProgress < 3 {
		t.Fatalf("expected repeated typed progress events, got %d: %s", inProgress, rr.Body.String())
	}
	if !completed {
		t.Fatalf("missing completed event: %s", rr.Body.String())
	}
}
