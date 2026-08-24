package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type toolEvidence struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
	HasResult bool   `json:"has_result"`
	Failed    bool   `json:"failed"`
}
type agentLedger struct {
	Completed           []toolEvidence `json:"completed"`
	Pending             []toolEvidence `json:"pending"`
	ToolRounds          int            `json:"tool_rounds"`
	RepeatedCall        bool           `json:"repeated_call"`
	RepeatedFailure     bool           `json:"repeated_failure"`
	RepetitionSignature string         `json:"repetition_signature,omitempty"`
}

var failureSignal = regexp.MustCompile(`(?i)(exit\s*(code|status)?\s*[:=]?\s*[1-9]\d*|\berrors?\b|\bfailed\b|\bfailure\b|exception|traceback|timed?\s*out|permission denied|not found|refused|失败|错误|异常|超时|拒绝|权限不足|未找到)`)
var explicitNoFailure = regexp.MustCompile(`(?i)(\bno\s+errors?\b|\bzero\s+errors?\b|\b0\s+errors?\b|\bwithout\s+errors?\b|\bexit\s*(code|status)?\s*[:=]?\s*0\b|没有错误|无错误|未发现错误)`)
var unsupportedSuccess = regexp.MustCompile(`(?i)\b(installed|created|written|executed|ran|started|deployed|deleted|verified|completed|succeeded|successful(?:ly)?)\b`)

func compactToolResult(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit < 200 {
		limit = 200
	}
	if len(s) <= limit {
		return s
	}
	head := limit / 3
	tail := limit - head - 80
	if tail < 80 {
		tail = 80
	}
	return s[:head] + fmt.Sprintf("\n... [truncated %d bytes] ...\n", len(s)-head-tail) + s[len(s)-tail:]
}
func scopedCallID(name, args string, index int, scope string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s:%s", scope, index, name, args)))
	return "call_" + hex.EncodeToString(h[:8])
}

func structuredToolFailure(content any) (bool, bool) {
	value := content
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if text == "" || json.Unmarshal([]byte(text), &value) != nil {
			return false, false
		}
	}
	m, ok := value.(map[string]any)
	if !ok {
		return false, false
	}
	fields := make(map[string]any, len(m))
	for key, field := range m {
		normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
		fields[normalized] = field
	}
	if value, ok := fields["iserror"].(bool); ok {
		return value, true
	}
	for _, key := range []string{"success", "ok"} {
		if value, ok := fields[key].(bool); ok {
			return !value, true
		}
	}
	number := func(value any) (float64, bool) {
		switch n := value.(type) {
		case float64:
			return n, true
		case float32:
			return float64(n), true
		case int:
			return float64(n), true
		case int64:
			return float64(n), true
		case json.Number:
			v, err := n.Float64()
			return v, err == nil
		case string:
			v, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
			return v, err == nil
		default:
			return 0, false
		}
	}
	for _, key := range []string{"exitcode", "exitstatus"} {
		if code, ok := number(fields[key]); ok {
			return code != 0, true
		}
	}
	for _, key := range []string{"statuscode", "httpstatus"} {
		if code, ok := number(fields[key]); ok {
			return code >= 400, true
		}
	}
	if status, ok := fields["status"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "ok", "success", "succeeded", "completed", "complete", "done", "passed":
			return false, true
		case "error", "failed", "failure", "timeout", "timed_out", "cancelled", "canceled", "denied":
			return true, true
		}
	}
	if value, exists := fields["error"]; exists {
		switch errorValue := value.(type) {
		case nil:
			return false, true
		case bool:
			return errorValue, true
		case string:
			return strings.TrimSpace(errorValue) != "", true
		default:
			return true, true
		}
	}
	return false, false
}

func toolResultFailed(content any, compact string) bool {
	if failed, decided := structuredToolFailure(content); decided {
		return failed
	}
	withoutExplicitSuccess := explicitNoFailure.ReplaceAllString(compact, "")
	return failureSignal.MatchString(withoutExplicitSuccess)
}

func buildAgentLedger(messages []oaiMsg) agentLedger {
	calls := map[string]toolEvidence{}
	order := []string{}
	toolRounds := 0
	for _, m := range messages {
		if m.Role == "assistant" {
			roundHasCall := false
			for _, raw := range m.ToolCalls {
				id, _ := raw["id"].(string)
				fn, _ := raw["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args := normalizeToolArguments(fmt.Sprint(fn["arguments"]))
				if id != "" {
					calls[id] = toolEvidence{ID: id, Name: name, Arguments: args}
					order = append(order, id)
					roundHasCall = true
				}
			}
			if roundHasCall {
				toolRounds++
			}
		}
		if m.Role == "tool" {
			e, ok := calls[m.ToolCallID]
			if !ok {
				continue
			}
			e.Result = compactToolResult(contentToString(m.Content), 4000)
			e.HasResult = true
			e.Failed = toolResultFailed(m.Content, e.Result)
			calls[m.ToolCallID] = e
		}
	}
	l := agentLedger{ToolRounds: toolRounds}
	seenFailure := map[string]int{}
	type callProgress struct {
		resultFingerprint string
		stagnantCount     int
	}
	progressByCall := map[string]callProgress{}
	for _, id := range order {
		e := calls[id]
		callSig := e.Name + "\x00" + e.Arguments
		if !e.HasResult {
			l.Pending = append(l.Pending, e)
		} else {
			l.Completed = append(l.Completed, e)
			fingerprint := strings.TrimSpace(e.Result)
			progress := progressByCall[callSig]
			if progress.stagnantCount > 0 && progress.resultFingerprint == fingerprint {
				progress.stagnantCount++
			} else {
				progress.resultFingerprint = fingerprint
				progress.stagnantCount = 1
			}
			progressByCall[callSig] = progress
			if progress.stagnantCount >= 3 {
				l.RepeatedCall = true
				if l.RepetitionSignature == "" {
					l.RepetitionSignature = callSig
				}
			}
			if e.Failed {
				sig := e.Name + "\x00" + e.Arguments + "\x00" + normalizeFailure(e.Result)
				seenFailure[sig]++
				if seenFailure[sig] >= 2 {
					l.RepeatedFailure = true
					l.RepetitionSignature = sig
				}
			}
		}
	}
	return l
}
func normalizeFailure(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`\d+`).ReplaceAllString(s, "#")
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

func normalizeToolArguments(arguments string) string {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return "{}"
	}
	var value any
	if json.Unmarshal([]byte(arguments), &value) == nil {
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
	}
	return arguments
}

func (l agentLedger) RecoveryInstruction() string {
	if !l.RepeatedFailure && !l.RepeatedCall {
		return ""
	}
	reason := "the same tool call returned the same result repeatedly without progress"
	if l.RepeatedFailure {
		reason = "the same tool call failed repeatedly"
	}
	return "\n[TOOL_LOOP_RECOVERY]\n" + reason + ". Do not issue that identical function and arguments again. Inspect the recorded failure, choose a materially different diagnostic or corrective call, or explain the concrete blocker to the user. Do not claim success without new successful evidence."
}

func (l agentLedger) blocksRepeatedToolCall(name, arguments string) bool {
	if (!l.RepeatedFailure && !l.RepeatedCall) || l.RepetitionSignature == "" {
		return false
	}
	parts := strings.SplitN(l.RepetitionSignature, "\x00", 3)
	if len(parts) < 2 {
		return false
	}
	return name == parts[0] && normalizeToolArguments(arguments) == normalizeToolArguments(parts[1])
}

func removeRepeatedToolCalls(calls []detectedToolCall, ledger agentLedger) ([]detectedToolCall, int) {
	if !ledger.RepeatedFailure && !ledger.RepeatedCall {
		return calls, 0
	}
	filtered := make([]detectedToolCall, 0, len(calls))
	removed := 0
	for _, call := range calls {
		if ledger.blocksRepeatedToolCall(call.Name, string(call.Arguments)) {
			removed++
			continue
		}
		filtered = append(filtered, call)
	}
	return filtered, removed
}
func (l agentLedger) RouterContext() string {
	return agentLedgerPrompt(l)
}

func agentLedgerPrompt(l agentLedger) string {
	type entry struct {
		ID         string `json:"call_id"`
		Name       string `json:"name"`
		Arguments  string `json:"arguments_sha256"`
		Status     string `json:"status"`
		Result     string `json:"result_sha256,omitempty"`
		FailureKey string `json:"failure,omitempty"`
	}
	entries := make([]entry, 0, len(l.Completed)+len(l.Pending))
	appendEntry := func(e toolEvidence, status string) {
		argHash := sha256.Sum256([]byte(e.Arguments))
		item := entry{ID: e.ID, Name: e.Name, Arguments: hex.EncodeToString(argHash[:8]), Status: status}
		if e.Result != "" {
			resultHash := sha256.Sum256([]byte(e.Result))
			item.Result = hex.EncodeToString(resultHash[:8])
		}
		if e.Failed {
			item.FailureKey = compactToolResult(normalizeFailure(e.Result), 240)
		}
		entries = append(entries, item)
	}
	for _, e := range l.Completed {
		status := "completed"
		if e.Failed {
			status = "failed"
		}
		appendEntry(e, status)
	}
	for _, e := range l.Pending {
		appendEntry(e, "pending")
	}
	payload, _ := json.Marshal(map[string]any{
		"tool_rounds": l.ToolRounds, "repeated_call": l.RepeatedCall,
		"repeated_failure": l.RepeatedFailure, "calls": entries,
	})
	return "[STRUCTURED_TOOL_LEDGER]\n" + string(payload)
}

func maxToolRounds() int {
	if raw, exists := os.LookupEnv("M365_MAX_TOOL_ROUNDS"); exists {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n > 0 && n <= 512 {
			return n
		}
		return 32
	}
	if n := currentSettings().MaxToolRounds; n > 0 && n <= 512 {
		return n
	}
	return 32
}

// activeMessages keeps only the current user turn and its follow-up tool loop.
// Older completed tool history must not block model switches or new user turns.
func activeMessages(messages []oaiMsg) []oaiMsg {
	lastUser := -1
	for i, m := range messages {
		if m.Role == "user" {
			lastUser = i
		}
	}
	if lastUser <= 0 {
		return messages
	}
	return messages[lastUser:]
}

func (l agentLedger) CanContinue(maxRounds int) error {
	if maxRounds <= 0 {
		maxRounds = 32
	}
	if l.ToolRounds >= maxRounds {
		return fmt.Errorf("tool round limit reached: %d", maxRounds)
	}
	if len(l.Pending) > 0 {
		return fmt.Errorf("pending tool results must be returned before another turn")
	}
	return nil
}

func completionEvidenceAllows(answer string, l agentLedger) bool {
	if len(l.Pending) > 0 {
		return false
	}
	if !unsupportedSuccess.MatchString(answer) {
		return true
	}
	for _, evidence := range l.Completed {
		if evidence.HasResult && !evidence.Failed {
			return true
		}
	}
	low := strings.ToLower(answer)
	for _, h := range []string{"cannot confirm", "not confirmed", "unable to confirm", "no tool result", "not completed", "failed"} {
		if strings.Contains(low, h) {
			return true
		}
	}
	return false
}
func completedCallIDs(l agentLedger) []string {
	o := make([]string, 0, len(l.Completed))
	for _, e := range l.Completed {
		o = append(o, e.ID)
	}
	sort.Strings(o)
	return o
}
