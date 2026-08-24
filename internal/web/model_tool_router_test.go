package web

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseModelToolDecisionAutoAndParallel(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[{"name":"get_weather","arguments":{"city":"Beijing"}},{"name":"get_time","arguments":{"city":"Beijing"}}]}`, testTools(), "auto")
	if !ok || len(calls) != 2 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}
func TestParseModelToolDecisionNoCall(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[]}`, testTools(), "auto")
	if !ok || len(calls) != 0 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}
func TestModelToolRouterPromptMarksCompletedResults(t *testing.T) {
	p := modelToolRouterPrompt(`assistant tool_calls: [...]
tool[call_x]: 2026-07-18`, testTools(), "auto")
	if !strings.Contains(p, "Completed evidence must not be repeated") || !strings.Contains(p, "tool[call_x]: 2026-07-18") || !strings.Contains(p, "unfinished work remains") {
		t.Fatalf("missing multi-turn evidence constraint: %s", p)
	}
}

func TestModelToolRouterPromptUsesExplicitShortcutBeforeAskingAgain(t *testing.T) {
	prompt := modelToolRouterPrompt(`Use C:\Work\server-shortcut.lnk to find example server #2.`, testTools(), "auto")
	for _, marker := range []string{"use that exact target", "do not ask the user to provide it again", ".lnk target is not source text", "do not Glob"} {
		if !strings.Contains(prompt, marker) {
			t.Fatalf("shortcut action rule missing %q: %s", marker, prompt)
		}
	}
}

func TestModelToolRouterContextKeepsInstructionsAndActiveTurn(t *testing.T) {
	longContract := "CONTRACT_HEAD " + strings.Repeat("policy ", 7000) + " CONTRACT_MIDDLE " + strings.Repeat("guardrail ", 4000) + " CONTRACT_TAIL"
	messages := []oaiMsg{
		{Role: "developer", Content: "WORK_UNTIL_VERIFIED " + longContract},
		{Role: "user", Content: "old task"},
		{Role: "assistant", Content: strings.Repeat("old answer ", 3000)},
		{Role: "user", Content: "CURRENT_OPEN_CODE_TASK"},
	}
	context := modelToolRouterContext(messages, "fallback", buildAgentLedger(messages))
	for _, marker := range []string{"WORK_UNTIL_VERIFIED", "CONTRACT_HEAD", "CONTRACT_MIDDLE", "CONTRACT_TAIL", "old task", "CURRENT_OPEN_CODE_TASK", "SYSTEM_AND_DEVELOPER_INSTRUCTIONS", "RECENT_USER_TASK_CONTEXT", "ACTIVE_TASK_AND_TOOL_EVIDENCE"} {
		if !strings.Contains(context, marker) {
			t.Fatalf("router context dropped %q", marker)
		}
	}
	if strings.Contains(context, "old answer") {
		t.Fatal("router context included unrelated old history")
	}
}

func TestModelToolRouterContextKeepsRecentReferentialTaskChain(t *testing.T) {
	messages := []oaiMsg{
		{Role: "developer", Content: "Work only inside the requested target."},
		{Role: "user", Content: `Inspect C:\Work\server-shortcut.lnk and repair example server #2.`},
		{Role: "assistant", Content: "I found the deployment source."},
		{Role: "user", Content: "继续修复，完成后测试"},
	}
	context := modelToolRouterContext(messages, "fallback", buildAgentLedger(messages))
	for _, marker := range []string{`C:\Work\server-shortcut.lnk`, "example server #2", "继续修复，完成后测试"} {
		if !strings.Contains(context, marker) {
			t.Fatalf("router context lost referential task marker %q: %s", marker, context)
		}
	}
}

func TestModelToolRouterContextKeepsAssistantPlanForStartTurn(t *testing.T) {
	messages := []oaiMsg{
		{Role: "developer", Content: "Continue autonomously after the user approves the plan."},
		{Role: "user", Content: "Repair the desktop application."},
		{Role: "assistant", Content: `Plan: update C:\Work\sample-project\app.ts, run the focused tests, then verify the packaged desktop build.`},
		{Role: "user", Content: "那就开始做"},
	}
	context := modelToolRouterContext(messages, "fallback", buildAgentLedger(messages))
	for _, marker := range []string{"RECENT_ASSISTANT_PLAN_AND_COMMITMENTS", `C:\Work\sample-project\app.ts`, "packaged desktop build", "那就开始做"} {
		if !strings.Contains(context, marker) {
			t.Fatalf("router context lost assistant plan marker %q: %s", marker, context)
		}
	}
}

func TestModelToolRouterPromptProtectsSourceCharacters(t *testing.T) {
	prompt := modelToolRouterPrompt("write an HTML file", testTools(), "auto")
	for _, marker := range []string{"Preserve every source-code character", `\u003c`, `\u003e`, `\u0026`} {
		if !strings.Contains(prompt, marker) {
			t.Fatalf("router prompt missing source-integrity rule %q: %s", marker, prompt)
		}
	}
}

func TestParseModelToolDecisionRestoresEscapedHTMLArguments(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[{"name":"get_weather","arguments":{"city":"\u003cdiv class=\"card\"\u003eA \u0026 B\u003c/div\u003e"}}]}`, testTools(), "auto")
	if !ok || len(calls) != 1 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
	var arguments map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &arguments); err != nil {
		t.Fatal(err)
	}
	if got := arguments["city"]; got != `<div class="card">A & B</div>` {
		t.Fatalf("escaped source was not restored: %#v", got)
	}
}

func TestNamedToolChoiceIsMandatoryAndRetryStaysNamed(t *testing.T) {
	choice := map[string]any{"type": "function", "name": "get_weather"}
	if !toolChoiceRequiresCall(choice) || toolChoiceRequiresCall("auto") {
		t.Fatal("named/auto requirement classification is wrong")
	}
	prompt := modelToolRouterPrompt("weather", testTools(), choice)
	if !strings.Contains(prompt, "MODE named:function_name") || !strings.Contains(prompt, "MODE: named:get_weather") {
		t.Fatalf("named mode is not constrained: %s", prompt)
	}
	retry := modelToolRequiredRetryPrompt("weather", testTools(), choice)
	if !strings.Contains(retry, "exactly one call to get_weather") || !strings.Contains(retry, "MODE: named:get_weather") {
		t.Fatalf("named retry is not constrained: %s", retry)
	}
}

func TestModelToolRouterToneUsesChatVariantWithinFamily(t *testing.T) {
	tests := map[string]string{
		"gpt-5.2-reasoning":       "Gpt_5_2_Chat",
		"gpt-5.3-reasoning":       "Gpt_5_3_Chat",
		"gpt-5.4-reasoning":       "Gpt_5_4_Chat",
		"gpt-5.5-reasoning":       "Gpt_5_5_Chat",
		"gpt-5.6-reasoning":       "Gpt_5_6_Chat",
		"gpt-5.6-sol":             "Gpt_5_6_Chat",
		"claude-sonnet-reasoning": "Claude_Sonnet",
	}
	for model, want := range tests {
		if got := modelToolRouterTone(model); got != want {
			t.Errorf("modelToolRouterTone(%q)=%q, want %q", model, got, want)
		}
	}
}

func TestParseModelToolDecisionRejectsBadSchema(t *testing.T) {
	calls, ok := parseModelToolDecision("```json\n{\"calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":2}}]}\n```", testTools(), "auto")
	if ok || len(calls) != 0 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}

func TestParseModelToolDecisionRejectsMixedValidAndInvalidCalls(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[{"name":"get_weather","arguments":{"city":"Beijing"}},{"name":"missing_tool","arguments":{}}]}`, testTools(), "auto")
	if ok || len(calls) != 0 {
		t.Fatalf("partially valid router output must be repaired atomically: calls=%v ok=%v", calls, ok)
	}
}

func TestParseModelToolDecisionRequiresExplicitCallsArray(t *testing.T) {
	for _, input := range []string{`{}`, `{"calls":null}`} {
		if calls, ok := parseModelToolDecision(input, testTools(), "auto"); ok || len(calls) != 0 {
			t.Fatalf("invalid no-op decision %s: calls=%v ok=%v", input, calls, ok)
		}
	}
}
