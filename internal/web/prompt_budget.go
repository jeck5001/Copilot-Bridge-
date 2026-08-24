package web

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

const (
	defaultInputBudgetTokens  = 96000
	promptBudgetReserveTokens = 2048
	minimumPromptBudgetTokens = 8192
	// defaultContinuingHistoryShare is the percentage of the prompt budget that
	// may be spent resending recent history even when the upstream ChatHub
	// conversation already holds it. Upstream trims its own internal context,
	// so relying on that memory alone made long agentic threads lose working
	// state mid-conversation ("the model forgets everything"). A bounded tail
	// keeps prompts well under the megabyte sizes that caused WebSocket 1006
	// drops while restoring continuity.
	defaultContinuingHistoryShare = 60
	maxTaskAnchorUnits            = 4
	maxTaskAnchorTokens           = 4096
)

var durableTaskAnchorPattern = regexp.MustCompile(`(?i)([a-z]:\\|\\\\[a-z0-9_.-]+\\|https?://|\.lnk\b|\bserver\s*#?\s*\d+\b|\d+\s*号服务器|(?:^|[\s"'])/(?:opt|home|root|srv|var|workspace|app|mnt)/)`)

// configuredContinuingHistoryShare returns the env-tunable history share
// (M365_CONTINUING_HISTORY_SHARE, 0..100).
func configuredContinuingHistoryShare() int64 {
	if raw := strings.TrimSpace(os.Getenv("M365_CONTINUING_HISTORY_SHARE")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n <= 100 {
			return int64(n)
		}
	}
	return defaultContinuingHistoryShare
}

type promptBudgetStats struct {
	OriginalMessages int
	SelectedMessages int
	DroppedMessages  int
	AnchoredMessages int
	PromptTokens     int64
	TaskAnchorTokens int64
	ToolTokens       int64
	AttachmentTokens int64
	PromptBudget     int64
	Continuing       bool
	Exceeded         bool
	ExceededReason   string
}

type promptUnit struct {
	messages    []oaiMsg
	tokens      int64
	instruction bool
	hasUser     bool
}

func attachmentBudgetTokens(model string, attachments []chathub.Attachment) int64 {
	serialized := mustJSON(attachments)
	// Exact BPE over large base64/data URLs is disproportionately expensive and
	// can pin a CPU for minutes under load. Large attachment payloads are already
	// non-natural text, so use a conservative linear estimate; small metadata
	// still uses the model tokenizer for accuracy.
	if len(serialized) > 32<<10 {
		return int64((len(serialized) + 1) / 2)
	}
	return countTokens(model, serialized)
}

func configuredInputBudgetTokens(model string) int64 {
	limit := int64(defaultInputBudgetTokens)
	if raw := strings.TrimSpace(os.Getenv("M365_INPUT_BUDGET_TOKENS")); raw != "" {
		// The configured value is the whole request budget. Leave room for the
		// protocol reserve as well as the minimum usable prompt window; accepting
		// 8192 here would make every request fail after subtracting the reserve.
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= minimumPromptBudgetTokens+promptBudgetReserveTokens {
			limit = n
		}
	}
	modelLimit := int64(modelLimitsFor(model).MaxInputTokens)
	if modelLimit > 0 && limit > modelLimit {
		limit = modelLimit
	}
	return limit
}

func promptMessageTokens(model string, m oaiMsg) int64 {
	chunk, attachments := renderPromptMessage(m)
	return countTokens(model, chunk) + countTokens(model, mustJSON(attachments))
}

// buildPromptUnits keeps an assistant tool call and its immediately following
// tool result(s) atomic. A tail-window trim therefore cannot leave a result
// without the call that gives its call_id meaning.
func buildPromptUnits(messages []oaiMsg, model string) []promptUnit {
	units := make([]promptUnit, 0, len(messages))
	for i := 0; i < len(messages); {
		m := messages[i]
		u := promptUnit{messages: []oaiMsg{m}}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		u.instruction = role == "system" || role == "developer"
		u.hasUser = role == "user"
		u.tokens = promptMessageTokens(model, m)
		i++
		if len(m.ToolCalls) == 0 {
			units = append(units, u)
			continue
		}
		for i < len(messages) && strings.EqualFold(strings.TrimSpace(messages[i].Role), "tool") {
			u.messages = append(u.messages, messages[i])
			u.tokens += promptMessageTokens(model, messages[i])
			i++
		}
		units = append(units, u)
	}
	return units
}

func durableTaskAnchorUnit(unit promptUnit) bool {
	if unit.instruction || len(unit.messages) != 1 {
		return false
	}
	message := unit.messages[0]
	role := strings.ToLower(strings.TrimSpace(message.Role))
	if role != "user" && role != "assistant" {
		return false
	}
	if len(message.ToolCalls) > 0 {
		return false
	}
	return durableTaskAnchorPattern.MatchString(contentToString(message.Content))
}

// selectPromptMessages applies a real global input budget. Production clients
// often resend hundreds of historical tool calls on every Responses request;
// forwarding that history verbatim made ChatHub receive megabyte-sized prompts
// and was the dominant cause of empty responses and WebSocket 1006 drops.
//
// Priority order is:
//  1. every system/developer instruction,
//  2. the current user turn and its tool loop,
//  3. the newest remaining conversation/tool units that still fit.
//
// When an upstream conversation is already persisted, older turns are still
// resent up to a bounded share of the budget (see
// configuredContinuingHistoryShare): upstream ChatHub trims its own internal
// context, so a current-turn-only prompt made long threads amnesiac. The cap
// keeps prompts far below the megabyte sizes that caused WebSocket 1006 drops.
func selectPromptMessages(messages []oaiMsg, model string, tools []chathub.Tool, attachments []chathub.Attachment, continuing bool) ([]oaiMsg, promptBudgetStats) {
	stats := promptBudgetStats{OriginalMessages: len(messages), Continuing: continuing}
	if len(messages) == 0 {
		return nil, stats
	}
	if len(tools) > 0 {
		stats.ToolTokens = countTokens(model, mustJSON(tools))
	}
	if len(attachments) > 0 {
		stats.AttachmentTokens = attachmentBudgetTokens(model, attachments)
	}
	totalBudget := configuredInputBudgetTokens(model)
	stats.PromptBudget = totalBudget - stats.ToolTokens - stats.AttachmentTokens - promptBudgetReserveTokens
	if stats.PromptBudget < minimumPromptBudgetTokens {
		stats.Exceeded = true
		stats.ExceededReason = "tool definitions or attachments leave too little prompt budget"
		return nil, stats
	}

	units := buildPromptUnits(messages, model)
	selected := make([]bool, len(units))
	used := int64(0)

	// System/developer instructions are part of the client's execution contract,
	// not optional history. Dropping an older instruction silently changes agent
	// behavior. Keep all of them in order and reject explicitly when the required
	// contract itself cannot fit.
	for i := len(units) - 1; i >= 0; i-- {
		if !units[i].instruction || units[i].tokens == 0 {
			continue
		}
		if used+units[i].tokens > stats.PromptBudget {
			stats.Exceeded = true
			stats.ExceededReason = "system/developer instructions exceed the configured input budget"
			return nil, stats
		}
		selected[i] = true
		used += units[i].tokens
	}

	lastUserUnit := 0
	for i := range units {
		if units[i].hasUser {
			lastUserUnit = i
		}
	}

	// The active user turn and its tool loop are indivisible. Sending only the
	// tail of an oversized active loop loses causal evidence and can make the
	// model repeat side effects, so reject it explicitly instead of truncating.
	currentTokens := int64(0)
	for i := lastUserUnit; i < len(units); i++ {
		if selected[i] || units[i].tokens == 0 {
			continue
		}
		currentTokens += units[i].tokens
	}
	if used+currentTokens > stats.PromptBudget {
		stats.Exceeded = true
		stats.ExceededReason = "current user turn exceeds the configured input budget"
		return nil, stats
	}
	for i := lastUserUnit; i < len(units); i++ {
		if !selected[i] && units[i].tokens > 0 {
			selected[i] = true
			used += units[i].tokens
		}
	}

	// Preserve a small set of durable task anchors outside the recent tail:
	// explicit local/remote paths, shortcut files, URLs, and numbered server
	// targets. Long OpenCode tasks can contain hundreds of tool messages; a
	// pure tail window previously dropped the original workspace/server path and
	// made the model search the current directory or ask the user again. Keep the
	// earliest anchor (usually the original task) plus the newest anchor updates,
	// while bounding their combined cost and never selecting tool-call units.
	anchorCandidates := make([]int, 0, maxTaskAnchorUnits)
	for i := 0; i < lastUserUnit; i++ {
		if durableTaskAnchorUnit(units[i]) {
			anchorCandidates = append(anchorCandidates, i)
		}
	}
	if len(anchorCandidates) > 0 {
		anchors := []int{anchorCandidates[0]}
		for i := len(anchorCandidates) - 1; i >= 0 && len(anchors) < maxTaskAnchorUnits; i-- {
			candidate := anchorCandidates[i]
			if candidate != anchors[0] {
				anchors = append(anchors, candidate)
			}
		}
		sort.Ints(anchors)
		for _, i := range anchors {
			if selected[i] || units[i].tokens == 0 || stats.TaskAnchorTokens+units[i].tokens > maxTaskAnchorTokens || used+units[i].tokens > stats.PromptBudget {
				continue
			}
			selected[i] = true
			used += units[i].tokens
			stats.TaskAnchorTokens += units[i].tokens
			stats.AnchoredMessages += len(units[i].messages)
		}
	}

	// Both fresh and continuing conversations get a recent-history tail. A
	// continuing conversation is capped at the configured share of the budget
	// because upstream already holds older turns; a fresh conversation may use
	// the whole budget. The tail walks backwards from the current turn and
	// stops at the first unit that no longer fits so the retained context is
	// always the most recent contiguous window (no time-line holes).
	historyLimit := stats.PromptBudget
	if continuing {
		historyLimit = stats.PromptBudget * configuredContinuingHistoryShare() / 100
	}
	for i := lastUserUnit - 1; i >= 0; i-- {
		if selected[i] || units[i].instruction || units[i].tokens == 0 {
			continue
		}
		if used+units[i].tokens > historyLimit {
			break
		}
		selected[i] = true
		used += units[i].tokens
	}

	out := make([]oaiMsg, 0, len(messages))
	for i, u := range units {
		if selected[i] {
			out = append(out, u.messages...)
		}
	}
	stats.SelectedMessages = len(out)
	stats.DroppedMessages = len(messages) - len(out)
	stats.PromptTokens = used
	return out, stats
}
