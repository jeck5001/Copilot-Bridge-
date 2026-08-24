package web

import (
	"encoding/json"
	"strings"
)

// toolProgress is transport metadata for a client-side long-running tool.
// It is intentionally not an oaiMsg: progress must never satisfy a pending
// tool call or trigger another model turn.
type toolProgress struct {
	CallID  string `json:"call_id"`
	Phase   string `json:"phase"`
	Message string `json:"message"`
	Output  string `json:"output,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

func parseToolProgress(v map[string]any) (toolProgress, bool) {
	if typ, _ := v["type"].(string); typ != "function_call_progress" {
		return toolProgress{}, false
	}
	p := toolProgress{}
	p.CallID, _ = v["call_id"].(string)
	p.Phase, _ = v["phase"].(string)
	p.Message, _ = v["message"].(string)
	p.Output, _ = v["output"].(string)
	p.Done, _ = v["done"].(bool)
	if strings.TrimSpace(p.CallID) == "" || strings.TrimSpace(p.Message) == "" {
		return toolProgress{}, false
	}
	return p, true
}

func progressJSON(p toolProgress) []byte {
	b, _ := json.Marshal(p)
	return b
}
