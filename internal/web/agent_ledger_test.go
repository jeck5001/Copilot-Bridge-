package web

import (
	"fmt"
	"strings"
	"testing"
)

func TestCompactToolResultKeepsHeadTailAndError(t *testing.T) {
	s := "start\n" + strings.Repeat("progress line\n", 1000) + "ERROR: build failed\nexit code 1"
	got := compactToolResult(s, 800)
	if len(got) > 900 || !strings.Contains(got, "start") || !strings.Contains(got, "ERROR: build failed") || !strings.Contains(got, "exit code 1") || !strings.Contains(got, "truncated") {
		t.Fatalf("bad compact result: %d %q", len(got), got)
	}
}

func TestAgentLedgerDetectsRepeatedFailure(t *testing.T) {
	l := buildAgentLedger([]oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":\"build\"}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "exit code 1: failed"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c2", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":\"build\"}"}}}},
		{Role: "tool", ToolCallID: "c2", Content: "exit code 1: failed"},
	})
	if !l.RepeatedFailure {
		t.Fatalf("expected repeated failure: %+v", l)
	}
	if err := l.CanContinue(32); err != nil {
		t.Fatalf("repeated failure should enter recovery instead of failing the whole API turn: %v", err)
	}
	if ctx := l.RouterContext(); !strings.Contains(ctx, "STRUCTURED_TOOL_LEDGER") || !strings.Contains(ctx, `"repeated_failure":true`) {
		t.Fatalf("RouterContext missing compact loop evidence, got %q", ctx)
	}
	if instruction := l.RecoveryInstruction(); !strings.Contains(instruction, "Do not issue that identical") {
		t.Fatalf("missing recovery instruction: %q", instruction)
	}
	blocked, removed := removeRepeatedToolCalls([]detectedToolCall{
		{Name: "run", Arguments: []byte(`{"cmd":"build"}`)},
		{Name: "inspect", Arguments: []byte(`{"path":"logs"}`)},
	}, l)
	if removed != 1 || len(blocked) != 1 || blocked[0].Name != "inspect" {
		t.Fatalf("repeated call filter removed the wrong calls: removed=%d calls=%+v", removed, blocked)
	}
}

func TestAgentLedgerEvidenceAndUniqueCallIDs(t *testing.T) {
	l := buildAgentLedger([]oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "create", "arguments": "{}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "created"},
	})
	if len(l.Completed) != 1 || l.Completed[0].ID != "c1" || l.Completed[0].Name != "create" || l.Completed[0].Result != "created" {
		t.Fatalf("missing evidence: %+v", l)
	}
	if len(l.Pending) != 0 {
		t.Fatalf("unexpected pending evidence: %+v", l)
	}
	first := scopedCallID("create", "{}", 0, "scope-a")
	second := scopedCallID("create", "{}", 0, "scope-b")
	if first == second {
		t.Fatalf("scoped call IDs should differ: %s", first)
	}
}

func TestAgentLedgerTreatsExplicitEmptyResultAsCompleted(t *testing.T) {
	l := buildAgentLedger([]oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "write", "arguments": "{}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: ""},
	})
	if len(l.Pending) != 0 || len(l.Completed) != 1 || !l.Completed[0].HasResult {
		t.Fatalf("explicit empty result was not completed evidence: %+v", l)
	}
	if err := l.CanContinue(2); err != nil {
		t.Fatalf("empty result incorrectly blocked the next turn: %v", err)
	}
	if context := l.RouterContext(); !strings.Contains(context, `"status":"completed"`) || strings.Contains(context, `"status":"pending"`) {
		t.Fatalf("empty result has wrong router status: %s", context)
	}
}

func TestAgentLedgerCountsParallelCallsAsOneRound(t *testing.T) {
	l := buildAgentLedger([]oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{
			{"id": "c1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}},
			{"id": "c2", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "a"},
		{Role: "tool", ToolCallID: "c2", Content: "b"},
	})
	if l.ToolRounds != 1 || len(l.Completed) != 2 {
		t.Fatalf("parallel calls must be one round: %+v", l)
	}
}

func TestAgentLedgerDetectsRepeatedCallAndRoundLimit(t *testing.T) {
	var msgs []oaiMsg
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "poll", "arguments": "{\"id\":1}"}}}}, oaiMsg{Role: "tool", ToolCallID: id, Content: "still pending"})
	}
	l := buildAgentLedger(msgs)
	if !l.RepeatedCall || l.ToolRounds != 4 {
		t.Fatalf("loop not detected: %+v", l)
	}
	if err := l.CanContinue(3); err == nil {
		t.Fatal("expected round limit")
	}
}

func TestAgentLedgerStopsSameSuccessfulCallWithoutProgress(t *testing.T) {
	var messages []oaiMsg
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("same%d", i)
		messages = append(messages,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "status", "arguments": `{"job":"a"}`}}}},
			oaiMsg{Role: "tool", ToolCallID: id, Content: `{"ok":true,"status":"completed","value":"unchanged"}`},
		)
	}
	ledger := buildAgentLedger(messages)
	if !ledger.RepeatedCall {
		t.Fatalf("same successful call/result was not recognized as no progress: %+v", ledger)
	}
	if err := ledger.CanContinue(32); err != nil {
		t.Fatalf("same successful call should be recovered inside the turn: %v", err)
	}
	if filtered, removed := removeRepeatedToolCalls([]detectedToolCall{{Name: "status", Arguments: []byte(`{"job":"a"}`)}}, ledger); removed != 1 || len(filtered) != 0 {
		t.Fatalf("same successful call was not suppressed: removed=%d filtered=%v", removed, filtered)
	}
}

func TestRepeatedToolCallMatchesSemanticallyEquivalentJSONArguments(t *testing.T) {
	ledger := buildAgentLedger([]oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "a", "function": map[string]any{"name": "run", "arguments": `{"path":"x","force":true}`}}}},
		{Role: "tool", ToolCallID: "a", Content: "failed: permission denied"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "b", "function": map[string]any{"name": "run", "arguments": `{"path":"x","force":true}`}}}},
		{Role: "tool", ToolCallID: "b", Content: "failed: permission denied"},
	})
	filtered, removed := removeRepeatedToolCalls([]detectedToolCall{{Name: "run", Arguments: []byte(`{"force":true,"path":"x"}`)}}, ledger)
	if removed != 1 || len(filtered) != 0 {
		t.Fatalf("equivalent JSON argument ordering bypassed loop guard: removed=%d filtered=%v", removed, filtered)
	}
}

func TestAgentLedgerAllowsPollingWhenResultChanges(t *testing.T) {
	var messages []oaiMsg
	for i, result := range []string{"queued", "running", "completed"} {
		id := fmt.Sprintf("poll%d", i)
		messages = append(messages,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "poll", "arguments": `{"job":"a"}`}}}},
			oaiMsg{Role: "tool", ToolCallID: id, Content: map[string]any{"success": true, "state": result}},
		)
	}
	ledger := buildAgentLedger(messages)
	if ledger.RepeatedCall || ledger.RepeatedFailure {
		t.Fatalf("changing poll results were treated as a loop: %+v", ledger)
	}
	if err := ledger.CanContinue(32); err != nil {
		t.Fatalf("progressing poll was blocked: %v", err)
	}
}

func TestAgentLedgerPrefersStructuredStatusAndUnderstandsChineseFailure(t *testing.T) {
	messages := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "ok", "type": "function", "function": map[string]any{"name": "build", "arguments": "{}"}}}},
		{Role: "tool", ToolCallID: "ok", Content: map[string]any{"exit_code": 0, "output": "no errors; historical error text only"}},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "bad", "type": "function", "function": map[string]any{"name": "deploy", "arguments": "{}"}}}},
		{Role: "tool", ToolCallID: "bad", Content: "部署失败：权限不足"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "string-code", "type": "function", "function": map[string]any{"name": "test", "arguments": "{}"}}}},
		{Role: "tool", ToolCallID: "string-code", Content: map[string]any{"exit_code": "2", "output": "process ended"}},
	}
	ledger := buildAgentLedger(messages)
	if len(ledger.Completed) != 3 || ledger.Completed[0].Failed || !ledger.Completed[1].Failed || !ledger.Completed[2].Failed {
		t.Fatalf("structured/Chinese result classification is wrong: %+v", ledger.Completed)
	}
	jsonFailure := buildAgentLedger([]oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "json-bad", "type": "function", "function": map[string]any{"name": "run", "arguments": "{}"}}}},
		{Role: "tool", ToolCallID: "json-bad", Content: `{"isError":true,"output":"looks successful"}`},
	})
	if len(jsonFailure.Completed) != 1 || !jsonFailure.Completed[0].Failed {
		t.Fatalf("JSON isError was ignored: %+v", jsonFailure)
	}
}

func TestActiveMessagesIgnoresOlderToolHistory(t *testing.T) {
	var msgs []oaiMsg
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("old%d", i)
		msgs = append(msgs,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "old", "arguments": "{}"}}}},
			oaiMsg{Role: "tool", ToolCallID: id, Content: "done"},
		)
	}
	msgs = append(msgs, oaiMsg{Role: "user", Content: "continue with a new model"})
	full := buildAgentLedger(msgs)
	active := buildAgentLedger(activeMessages(msgs))
	if full.ToolRounds < 20 {
		t.Fatalf("expected full history tools, got %d", full.ToolRounds)
	}
	if active.ToolRounds != 0 {
		t.Fatalf("new user turn should reset round limit scope, got %d", active.ToolRounds)
	}
	if err := active.CanContinue(16); err != nil {
		t.Fatalf("new user turn blocked by old history: %v", err)
	}
}

func TestCompletionGuardRejectsPendingAndUnsupportedSuccess(t *testing.T) {
	l := buildAgentLedger([]oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{
			{"id": "p1", "type": "function", "function": map[string]any{"name": "deploy", "arguments": "{}"}},
		}},
	})
	if completionEvidenceAllows("Deployment completed successfully", l) {
		t.Fatal("pending action allowed as complete")
	}
}

func TestCompletionGuardRejectsUnsupportedSuccess(t *testing.T) {
	if completionEvidenceAllows("Installed, started, and verified successfully", buildAgentLedger(nil)) {
		t.Fatal("unsupported success allowed")
	}
	if !completionEvidenceAllows("I cannot confirm completion because no tool results were returned.", buildAgentLedger(nil)) {
		t.Fatal("honest incomplete response rejected")
	}
}

func TestCompletionGuardDistinguishesFailedAndSuccessfulEvidence(t *testing.T) {
	failed := agentLedger{Completed: []toolEvidence{{ID: "call_failed", HasResult: true, Failed: true}}}
	if completionEvidenceAllows("Deployment completed successfully", failed) {
		t.Fatal("failed tool result was treated as successful completion evidence")
	}
	if !completionEvidenceAllows("Deployment failed; I cannot confirm completion.", failed) {
		t.Fatal("honest failure report was rejected")
	}
	emptySuccess := agentLedger{Completed: []toolEvidence{{ID: "call_empty", HasResult: true}}}
	if !completionEvidenceAllows("Deployment completed successfully", emptySuccess) {
		t.Fatal("successful empty tool result was rejected")
	}
	mixed := agentLedger{Completed: []toolEvidence{
		{ID: "call_failed", HasResult: true, Failed: true},
		{ID: "call_ok", HasResult: true},
	}}
	if !completionEvidenceAllows("Deployment completed successfully", mixed) {
		t.Fatal("successful evidence in a mixed result set was ignored")
	}
}

func TestRouterContextDoesNotExposeRawToolData(t *testing.T) {
	prompt := agentLedger{
		Completed: []toolEvidence{{ID: "call_1", Name: "exec_command", Arguments: "{\"cmd\":\"cat /secret\"}", Result: "SECRET_OUTPUT"}},
		Pending:   []toolEvidence{{ID: "call_2", Name: "write_stdin", Arguments: "{\"session_id\":123}", Result: ""}},
	}.RouterContext()
	if prompt == "" {
		t.Fatal("RouterContext must retain non-sensitive structured evidence")
	}
	for _, required := range []string{"STRUCTURED_TOOL_LEDGER", "call_1", "call_2", "exec_command", "write_stdin", "arguments_sha256", "result_sha256"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("tool prompt lost %q in %q", required, prompt)
		}
	}
	for _, forbidden := range []string{"cat /secret", "SECRET_OUTPUT", "session_id"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("tool prompt leaked %q in %q", forbidden, prompt)
		}
	}
}
