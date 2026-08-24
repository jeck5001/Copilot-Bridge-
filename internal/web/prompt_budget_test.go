package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

func TestPromptBudgetProductionScaleHistoryStaysGloballyBounded(t *testing.T) {
	t.Setenv("M365_INPUT_BUDGET_TOKENS", "12000")
	msgs := []oaiMsg{{Role: "developer", Content: "do not lose this policy"}}
	for i := 0; i < 31; i++ {
		msgs = append(msgs, oaiMsg{Role: "user", Content: fmt.Sprintf("old question %d", i)}, oaiMsg{Role: "assistant", Content: fmt.Sprintf("old answer %d", i)})
	}
	for i := 0; i < 283; i++ {
		id := fmt.Sprintf("historic_%03d", i)
		msgs = append(msgs,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "inspect", "arguments": fmt.Sprintf(`{"item":%d}`, i)}}}},
			oaiMsg{Role: "tool", ToolCallID: id, Content: strings.Repeat("historic-result-", 430)})
	}
	msgs = append(msgs, oaiMsg{Role: "user", Content: "CURRENT QUESTION"})
	if len(msgs) != 630 {
		t.Fatalf("fixture drifted: got %d items", len(msgs))
	}
	raw, err := json.Marshal(msgs)
	if err != nil || len(raw) < 1_800_000 {
		t.Fatalf("fixture must exceed 1.8 MB: bytes=%d err=%v", len(raw), err)
	}
	got, stats := selectPromptMessages(msgs, "m365-copilot", nil, nil, false)
	prompt, _ := flattenPromptMessages(got, nil)
	if !strings.Contains(prompt, "CURRENT QUESTION") || !strings.Contains(prompt, "do not lose this policy") {
		t.Fatal("current request or instruction was dropped")
	}
	if stats.DroppedMessages < 500 || countTokens("m365-copilot", prompt) > stats.PromptBudget {
		t.Fatalf("history not globally bounded: %+v", stats)
	}
	if err := validateToolConversation(got); err != nil {
		t.Fatalf("trim broke tool groups: %v", err)
	}
}

func TestPromptBudgetNeverSplitsParallelToolCallUnit(t *testing.T) {
	t.Setenv("M365_INPUT_BUDGET_TOKENS", "12000")
	msgs := []oaiMsg{{Role: "user", Content: "old turn"}}
	for round := 0; round < 12; round++ {
		calls := make([]map[string]any, 0, 3)
		for call := 0; call < 3; call++ {
			id := fmt.Sprintf("r%d_c%d", round, call)
			calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "inspect", "arguments": `{}`}})
		}
		msgs = append(msgs, oaiMsg{Role: "assistant", ToolCalls: calls})
		for call := 0; call < 3; call++ {
			msgs = append(msgs, oaiMsg{Role: "tool", ToolCallID: fmt.Sprintf("r%d_c%d", round, call), Content: strings.Repeat("evidence-", 900)})
		}
	}
	msgs = append(msgs, oaiMsg{Role: "user", Content: "new turn"})
	got, stats := selectPromptMessages(msgs, "m365-copilot", nil, nil, false)
	if stats.DroppedMessages == 0 {
		t.Fatal("fixture did not force a trim")
	}
	if err := validateToolConversation(got); err != nil {
		t.Fatalf("parallel group was split: %v", err)
	}
	for i, m := range got {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		if len(m.ToolCalls) != 3 || i+3 >= len(got) {
			t.Fatalf("partial tool group retained at %d", i)
		}
		for j, call := range m.ToolCalls {
			id, _ := call["id"].(string)
			if got[i+1+j].Role != "tool" || got[i+1+j].ToolCallID != id {
				t.Fatalf("call/result adjacency broken for %q", id)
			}
		}
	}
}

func TestPromptBudgetDropsOldToolHistoryAndKeepsCurrentTurn(t *testing.T) {
	t.Setenv("M365_INPUT_BUDGET_TOKENS", "12000")
	msgs := []oaiMsg{{Role: "developer", Content: "keep this instruction"}}
	for i := 0; i < 180; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "inspect", "arguments": `{"path":"x"}`}}}},
			oaiMsg{Role: "tool", ToolCallID: id, Content: strings.Repeat("old-result-", 900)},
		)
	}
	msgs = append(msgs, oaiMsg{Role: "user", Content: "CURRENT QUESTION"})
	tools := []chathub.Tool{{Type: "function", Function: []byte(`{"name":"inspect","parameters":{"type":"object"}}`)}}

	got, stats := selectPromptMessages(msgs, "gpt-5.6-reasoning", tools, nil, false)
	prompt, _ := flattenPromptMessages(got, nil)
	if !strings.Contains(prompt, "keep this instruction") || !strings.Contains(prompt, "CURRENT QUESTION") {
		t.Fatalf("priority content missing from trimmed prompt")
	}
	if stats.DroppedMessages == 0 || stats.SelectedMessages >= stats.OriginalMessages {
		t.Fatalf("history was not trimmed: %+v", stats)
	}
	if countTokens("gpt-5.6-reasoning", prompt) > stats.PromptBudget+256 {
		t.Fatalf("prompt exceeded budget: tokens=%d stats=%+v", countTokens("gpt-5.6-reasoning", prompt), stats)
	}
	if err := validateToolConversation(got); err != nil {
		t.Fatalf("trim broke tool call/result pairing: %v", err)
	}
}

func TestPromptBudgetContinuingSessionResendsBoundedTail(t *testing.T) {
	t.Setenv("M365_CONTINUING_HISTORY_SHARE", "60")
	msgs := []oaiMsg{
		{Role: "developer", Content: "policy"},
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "new question"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "function": map[string]any{"name": "f", "arguments": `{}`}}}},
		{Role: "tool", ToolCallID: "c1", Content: "new evidence"},
	}
	got, stats := selectPromptMessages(msgs, "gpt-5.6-reasoning", nil, nil, true)
	prompt, _ := flattenPromptMessages(got, nil)
	// The recent-history tail must be resent so the model keeps working state
	// even when upstream trims its own conversation context.
	for _, want := range []string{"policy", "old question", "old answer", "new question", "new evidence"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q from continuing prompt: %s", want, prompt)
		}
	}
	if stats.PromptTokens > stats.PromptBudget {
		t.Fatalf("continuing prompt exceeded budget: tokens=%d budget=%d", stats.PromptTokens, stats.PromptBudget)
	}
	if !stats.Continuing {
		t.Fatal("continuing flag not reported")
	}
}

func TestPromptBudgetContinuingShareZeroKeepsActiveTurnOnly(t *testing.T) {
	t.Setenv("M365_CONTINUING_HISTORY_SHARE", "0")
	msgs := []oaiMsg{
		{Role: "developer", Content: "policy"},
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "new question"},
	}
	got, _ := selectPromptMessages(msgs, "gpt-5.6-reasoning", nil, nil, true)
	prompt, _ := flattenPromptMessages(got, nil)
	if strings.Contains(prompt, "old question") || strings.Contains(prompt, "old answer") {
		t.Fatalf("history was resent despite zero share: %s", prompt)
	}
	for _, want := range []string{"policy", "new question"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q from continuing prompt", want)
		}
	}
}

func TestPromptBudgetRejectsOversizedCurrentTurn(t *testing.T) {
	t.Setenv("M365_INPUT_BUDGET_TOKENS", "12000")
	msgs := []oaiMsg{{Role: "user", Content: strings.Repeat("oversized-current-word ", 50000)}}
	got, stats := selectPromptMessages(msgs, "gpt-5.6-reasoning", nil, nil, false)
	if !stats.Exceeded || len(got) != 0 || !strings.Contains(stats.ExceededReason, "current user turn") {
		t.Fatalf("oversized current turn was not rejected: got=%d stats=%+v", len(got), stats)
	}
}

func TestPromptBudgetKeepsInstructionsBeyondOldOneThirdCap(t *testing.T) {
	t.Setenv("M365_INPUT_BUDGET_TOKENS", "12000")
	policy := "POLICY_START " + strings.Repeat("must keep working and verify results ", 650) + " POLICY_END"
	got, stats := selectPromptMessages([]oaiMsg{{Role: "developer", Content: policy}, {Role: "user", Content: "CURRENT_TASK"}}, "gpt-5.6-sol", nil, nil, false)
	if stats.Exceeded {
		t.Fatalf("required context unexpectedly exceeded budget: %+v", stats)
	}
	prompt, _ := flattenPromptMessages(got, nil)
	for _, marker := range []string{"POLICY_START", "POLICY_END", "CURRENT_TASK"} {
		if !strings.Contains(prompt, marker) {
			t.Fatalf("required context dropped %q", marker)
		}
	}
}

func TestPromptBudgetRejectsOversizedInstructionsExplicitly(t *testing.T) {
	t.Setenv("M365_INPUT_BUDGET_TOKENS", "12000")
	got, stats := selectPromptMessages([]oaiMsg{{Role: "developer", Content: strings.Repeat("policy-word ", 50000)}, {Role: "user", Content: "task"}}, "gpt-5.6-sol", nil, nil, false)
	if !stats.Exceeded || len(got) != 0 || !strings.Contains(stats.ExceededReason, "instructions") {
		t.Fatalf("oversized instructions were silently dropped: got=%d stats=%+v", len(got), stats)
	}
}

func TestCurrentToolResultIsNotSilentlyTruncated(t *testing.T) {
	marker := "MIDDLE_COMPILER_ERROR_MUST_SURVIVE"
	result := strings.Repeat("A", 6000) + marker + strings.Repeat("B", 6000)
	messages := []oaiMsg{
		{Role: "user", Content: "fix the build"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_build", "type": "function", "function": map[string]any{"name": "exec", "arguments": `{}`}}}},
		{Role: "tool", ToolCallID: "call_build", Content: result},
	}
	got, stats := selectPromptMessages(messages, "gpt-5.6-sol", nil, nil, false)
	if stats.Exceeded {
		t.Fatalf("fixture should fit: %+v", stats)
	}
	prompt, _ := flattenPromptMessages(got, nil)
	if !strings.Contains(prompt, marker) || strings.Contains(prompt, "[truncated") {
		t.Fatalf("current tool result was silently truncated")
	}
}

func TestPromptBudgetCountsExplicitAttachments(t *testing.T) {
	t.Setenv("M365_INPUT_BUDGET_TOKENS", "12000")
	attachments := []chathub.Attachment{{Type: "image", URL: "data:image/png;base64," + strings.Repeat("A", 200000)}}
	got, stats := selectPromptMessages([]oaiMsg{{Role: "user", Content: "inspect"}}, "gpt-5.6-reasoning", nil, attachments, false)
	if !stats.Exceeded || len(got) != 0 || stats.AttachmentTokens == 0 {
		t.Fatalf("oversized attachment was not rejected: got=%d stats=%+v", len(got), stats)
	}
}

func TestPromptBudgetKeepsOriginalShortcutAndServerAnchorInLongOpenCodeTask(t *testing.T) {
	t.Setenv("M365_INPUT_BUDGET_TOKENS", "12000")
	messages := []oaiMsg{
		{Role: "developer", Content: "Work autonomously and use available tools before asking the user."},
		{Role: "user", Content: `Use C:\Work\server-shortcut.lnk to connect to example server #2 and improve the deployed website.`},
	}
	for i := 0; i < 90; i++ {
		id := fmt.Sprintf("historic_%d", i)
		messages = append(messages,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "glob", "arguments": `{"pattern":"**/*"}`}}}},
			oaiMsg{Role: "tool", ToolCallID: id, Content: strings.Repeat("large historic result ", 260)},
		)
	}
	messages = append(messages, oaiMsg{Role: "user", Content: "继续优化视觉效果和自适应"})

	got, stats := selectPromptMessages(messages, "gpt-5.6-sol", nil, nil, false)
	prompt, _ := flattenPromptMessages(got, nil)
	for _, marker := range []string{`C:\Work\server-shortcut.lnk`, "example server #2", "继续优化视觉效果和自适应"} {
		if !strings.Contains(prompt, marker) {
			t.Fatalf("long task lost durable marker %q: stats=%+v", marker, stats)
		}
	}
	if stats.AnchoredMessages != 1 || stats.TaskAnchorTokens == 0 || stats.DroppedMessages == 0 {
		t.Fatalf("anchor accounting or trim is wrong: %+v", stats)
	}
	if countTokens("gpt-5.6-sol", prompt) > stats.PromptBudget {
		t.Fatalf("anchored prompt exceeded budget: tokens=%d stats=%+v", countTokens("gpt-5.6-sol", prompt), stats)
	}
	if err := validateToolConversation(got); err != nil {
		t.Fatalf("anchor retention split a tool unit: %v", err)
	}
}
