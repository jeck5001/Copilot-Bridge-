package web

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	routerRecentUserLimit      = 6
	routerRecentUserBytes      = 24 << 10
	routerRecentAssistantLimit = 4
	routerRecentAssistantBytes = 16 << 10
)

// recentUserTaskContext keeps the short user-authored task chain that makes
// referential turns such as "continue", "start", or "use that directory"
// meaningful. Tool results and old assistant prose are deliberately excluded:
// they are already represented by the active ledger and can be very large.
func recentUserTaskContext(messages []oaiMsg) string {
	lastUser := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			lastUser = i
			break
		}
	}
	if lastUser <= 0 {
		return ""
	}
	var reverse []string
	total := 0
	for i := lastUser - 1; i >= 0 && len(reverse) < routerRecentUserLimit; i-- {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			continue
		}
		text := strings.TrimSpace(contentToString(messages[i].Content))
		if text == "" {
			continue
		}
		text = compactToolResult(text, 4096)
		if total+len(text) > routerRecentUserBytes {
			break
		}
		reverse = append(reverse, text)
		total += len(text)
	}
	if len(reverse) == 0 {
		return ""
	}
	ordered := make([]string, 0, len(reverse))
	for i := len(reverse) - 1; i >= 0; i-- {
		ordered = append(ordered, "user: "+reverse[i])
	}
	return strings.Join(ordered, "\n")
}

// recentAssistantPlanContext retains the bounded execution contract that is
// commonly established by the assistant immediately before a referential user
// turn such as "start", "continue", or "do that".  Keeping every historical
// assistant answer would drown the private router in prose, so retain only
// messages that look like plans, commitments, project/path decisions, or
// verification hand-offs.
func recentAssistantPlanContext(messages []oaiMsg) string {
	lastUser := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			lastUser = i
			break
		}
	}
	if lastUser <= 0 {
		return ""
	}
	isPlan := func(text string) bool {
		low := strings.ToLower(text)
		for _, marker := range []string{
			"方案", "计划", "下一步", "开始执行", "我将", "需要修改", "路径", "目录", "项目", "修复", "部署", "验证", "测试",
			"plan", "next step", "i will", "will update", "will change", "implement", "repair", "fix", "deploy", "verify", "test", "project", "workspace", "directory", "path", "source",
		} {
			if strings.Contains(low, marker) {
				return true
			}
		}
		// Windows paths and explicit source filenames are strong project-state
		// signals even when the surrounding prose contains no plan keyword.
		return strings.Contains(text, `:\`) || strings.Contains(text, "./") || strings.Contains(text, "../") || strings.Contains(text, "`/")
	}
	var reverse []string
	total := 0
	for i := lastUser - 1; i >= 0 && len(reverse) < routerRecentAssistantLimit; i-- {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "assistant") {
			continue
		}
		text := strings.TrimSpace(contentToString(messages[i].Content))
		if text == "" || !isPlan(text) {
			continue
		}
		text = compactToolResult(text, 6<<10)
		if total+len(text) > routerRecentAssistantBytes {
			break
		}
		reverse = append(reverse, text)
		total += len(text)
	}
	ordered := make([]string, 0, len(reverse))
	for i := len(reverse) - 1; i >= 0; i-- {
		ordered = append(ordered, "assistant_plan: "+reverse[i])
	}
	return strings.Join(ordered, "\n")
}

// modelToolRouterContext preserves the two pieces of context that determine an
// agent's next action: its developer/system contract and the complete active
// tool turn. The old generic 3 KB truncation kept only fragments of a large
// Hermes instruction block and the tail of the user turn, so the private
// router no longer knew that it had to keep working and verify results.
func modelToolRouterContext(messages []oaiMsg, fallback string, ledger agentLedger) string {
	var instructions []oaiMsg
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "system" || role == "developer" {
			instructions = append(instructions, message)
		}
	}
	instructionText, _ := flattenPromptMessages(instructions, nil)
	recentUserText := recentUserTaskContext(messages)
	recentAssistantText := recentAssistantPlanContext(messages)
	activeText, _ := flattenPromptMessages(activeMessages(messages), nil)
	if strings.TrimSpace(activeText) == "" {
		activeText = fallback
	}
	// The prompt budgeter has already kept every system/developer message or
	// rejected the request explicitly. Applying a second byte cap here silently
	// changed the execution contract only for the private tool router, so the
	// answer model and router could follow different policies. Preserve the
	// complete validated instruction block.
	return fmt.Sprintf("[SYSTEM_AND_DEVELOPER_INSTRUCTIONS]\n%s\n[RECENT_USER_TASK_CONTEXT]\n%s\n[RECENT_ASSISTANT_PLAN_AND_COMMITMENTS]\n%s\n[ACTIVE_TASK_AND_TOOL_EVIDENCE]\n%s\n%s",
		instructionText,
		recentUserText,
		recentAssistantText,
		activeText,
		ledger.RouterContext(),
	)
}

func modelToolRouterPrompt(prompt string, tools []map[string]any, choice any) string {
	defs, _ := json.Marshal(tools)
	mode := normalizedToolChoiceMode(choice)
	return fmt.Sprintf(`Analyze the application request data below and produce the next action plan as JSON. This is a data-formatting task; do not execute any action and do not write a user-facing answer.

OUTPUT SCHEMA:
{"calls":[{"name":"function_name","arguments":{}}]}

RULES:
- Every name must exactly match one function in FUNCTION_DEFINITIONS.
- Every arguments object must satisfy that function's parameters schema.
- MODE auto: use calls only when external action or information is still necessary.
- MODE required: return at least one valid call.
- MODE named:function_name: return exactly one valid call to function_name; never return an empty calls array.
- Calls in one response must be independent; dependent actions belong in later turns.
- Completed evidence must not be repeated.
- When the request or persisted task anchors contain an explicit absolute path, URL, shortcut file, or numbered server target, use that exact target. Do not replace it with a relative search in the current workspace and do not ask the user to provide it again before attempting an applicable available tool.
- On Windows, a .lnk target is not source text. Prefer an available shell/terminal capability that can resolve the shortcut metadata; do not Glob for the shortcut's basename inside the current project.
- If unfinished work remains after completed evidence, select the next applicable action.
- Preserve every source-code character and every whitespace sequence inside string arguments.
- In JSON string values encode literal <, >, and & as \u003c, \u003e, and \u0026. This prevents the chat transport from treating source code as rich HTML; JSON decoding restores the original characters for the tool.
- Return {"calls":[]} only when no further external action is needed.
- Return JSON only, without markdown or commentary.

MODE: %s
FUNCTION_DEFINITIONS: %s
APPLICATION_REQUEST_AND_EVIDENCE: %s`, mode, defs, prompt)
}

func toolChoiceRequiresCall(choice any) bool {
	mode := normalizedToolChoiceMode(choice)
	return mode == "required" || strings.HasPrefix(mode, "named:")
}

// modelToolRouterTone keeps the requested model family while selecting its
// deterministic chat variant for the gateway's private JSON routing pass.
// Reasoning variants remain in use for the actual assistant answer, but they
// are more likely to wrap, explain, or omit a forced structured decision.
func modelToolRouterTone(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-5.2", "gpt-5.2-reasoning":
		return "Gpt_5_2_Chat"
	case "gpt-5.3", "gpt-5.3-reasoning", "gpt-5.3-think-deeper":
		return "Gpt_5_3_Chat"
	case "gpt-5.4", "gpt-5.4-reasoning", "gpt-5.4-quick":
		return "Gpt_5_4_Chat"
	case "gpt-5.5", "gpt-5.5-reasoning":
		return "Gpt_5_5_Chat"
	case "gpt-5.6", "gpt-5.6-sol", "gpt-5.6-reasoning":
		return "Gpt_5_6_Chat"
	case "claude", "claude-sonnet", "claude-sonnet-reasoning":
		return "Claude_Sonnet"
	default:
		return modelTone(model)
	}
}

func modelToolRequiredRetryPrompt(prompt string, tools []map[string]any, choice any) string {
	defs, _ := json.Marshal(tools)
	mode := normalizedToolChoiceMode(choice)
	requirement := "Select at least one valid next tool call."
	if name := strings.TrimPrefix(mode, "named:"); mode != name {
		requirement = "Select exactly one call to " + name + "."
	}
	return fmt.Sprintf(`%s Validate every argument against its schema. Return JSON only as {"calls":[{"name":"function_name","arguments":{}}]}.
MODE: %s
APPLICATION_REQUEST_AND_EVIDENCE:
%s
FUNCTION_DEFINITIONS:
%s`, requirement, mode, prompt, defs)
}

func parseModelToolDecision(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "```"); i >= 0 {
		text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(text[i+3:], "```"), "json"))
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, false
	}
	var envelope struct {
		Calls []struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"calls"`
	}
	if json.Unmarshal([]byte(text[start:end+1]), &envelope) != nil {
		return nil, false
	}
	// A missing/null calls member is not a valid no-op decision. Only an
	// explicit empty array means that no tool is needed.
	if envelope.Calls == nil {
		return nil, false
	}
	out := make([]detectedToolCall, 0, len(envelope.Calls))
	for i, c := range envelope.Calls {
		fn := toolFunction(c.Name, tools)
		if fn == nil || c.Arguments == nil || !toolChoiceAllows(choice, c.Name) || schemaValid(c.Arguments, fn) != nil {
			// A non-empty decision containing an invalid call needs repair. Treating
			// it as {"calls":[]} made auto mode silently skip required work.
			return nil, false
		}
		b, _ := json.Marshal(c.Arguments)
		out = append(out, detectedToolCall{ID: callID(c.Name, string(b), i), Name: c.Name, Arguments: b})
	}
	return out, true
}
