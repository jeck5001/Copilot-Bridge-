package web

import (
	"fmt"
	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
	"strings"
)

// flattenPromptMessages adapts role-based messages to ChatHub's single text field
// without losing instruction priority or tool-call identity.
func flattenPromptMessages(messages []oaiMsg, attachments []chathub.Attachment) (string, []chathub.Attachment) {
	var instructions []string
	var transcript strings.Builder
	finalUser := -1
	if len(messages) > 0 {
		last := messages[len(messages)-1]
		if strings.EqualFold(strings.TrimSpace(last.Role), "user") && len(last.ToolCalls) == 0 {
			finalUser = len(messages) - 1
		}
	}
	finalPrompt := ""
	lastEvidenceRole := ""
	for i, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			text, files := parseContent(m.Content)
			attachments = append(attachments, files...)
			if text = strings.TrimSpace(text); text != "" {
				instructions = append(instructions, text)
			}
			continue
		}
		if i == finalUser {
			text, files := parseContent(m.Content)
			attachments = append(attachments, files...)
			finalPrompt = strings.TrimSpace(text)
			continue
		}
		chunk, files := renderPromptMessage(m)
		attachments = append(attachments, files...)
		transcript.WriteString(chunk)
		if strings.TrimSpace(chunk) != "" {
			lastEvidenceRole = role
		}
	}

	// Follow the captured M365 bridge shape: system/developer content is an
	// additional-context preamble, earlier messages are a transcript, and the
	// current user request remains the plain final prompt after a neutral
	// separator. Repeating role labels or appending "authoritative system"
	// jailbreak-like text materially increases ChatHub Disengaged frames.
	var context []string
	if len(instructions) > 0 {
		context = append(context, "System instructions:\n"+strings.Join(instructions, "\n"))
	}
	if history := strings.TrimSpace(transcript.String()); history != "" {
		header := "Prior conversation transcript:"
		if lastEvidenceRole == "tool" {
			header = "Conversation transcript and current tool evidence:"
		}
		context = append(context, header+"\n"+history)
	}
	if finalPrompt == "" {
		return strings.TrimSpace(strings.Join(context, "\n\n")), attachments
	}
	if len(context) == 0 {
		return finalPrompt, attachments
	}
	return strings.TrimSpace(strings.Join(context, "\n\n") + "\n\n---\n\n" + finalPrompt), attachments
}

// renderPromptMessage is deliberately shared by the context budgeter and the
// final ChatHub adapter. Keeping one renderer prevents the budget estimate from
// drifting away from the bytes actually sent upstream.
func renderPromptMessage(m oaiMsg) (string, []chathub.Attachment) {
	role := strings.ToLower(strings.TrimSpace(m.Role))
	if role == "" {
		role = "user"
	}
	txt, files := parseContent(m.Content)
	txt = strings.TrimSpace(txt)
	if len(m.ToolCalls) > 0 {
		var b strings.Builder
		if txt != "" {
			b.WriteString(fmt.Sprintf("\n[%s]\n%s\n", role, txt))
		}
		b.WriteString(fmt.Sprintf("\n[%s tool_calls]\n%s\n", role, mustJSON(m.ToolCalls)))
		return b.String(), files
	}
	if role == "tool" {
		return fmt.Sprintf("\n[tool result id=%s]\n%s\n", m.ToolCallID, txt), files
	}
	if txt == "" {
		return "", files
	}
	return fmt.Sprintf("\n[%s]\n%s\n", role, txt), files
}
