package web

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type modelLimits struct{ ContextWindow, MaxInputTokens, MaxOutputTokens int }
type reasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type modelSpec struct {
	ID, Owner                                      string
	Tools, Reasoning                               bool
	ContextWindow, MaxInputTokens, MaxOutputTokens int
}

var gatewayModels = []modelSpec{
	{ID: "gpt-5.2", Owner: "microsoft-365", Tools: true, Reasoning: true, ContextWindow: 400000, MaxOutputTokens: 128000},
	{ID: "gpt-5.2-reasoning", Owner: "microsoft-365", Tools: true, Reasoning: true, ContextWindow: 400000, MaxOutputTokens: 128000},
	{ID: "gpt-5.3", Owner: "microsoft-365", Tools: true, Reasoning: false, ContextWindow: 400000, MaxOutputTokens: 128000},
	{ID: "gpt-5.4", Owner: "microsoft-365", Tools: true, Reasoning: true, ContextWindow: 1050000, MaxOutputTokens: 128000},
	{ID: "gpt-5.4-reasoning", Owner: "microsoft-365", Tools: true, Reasoning: true, ContextWindow: 1050000, MaxOutputTokens: 128000},
	{ID: "gpt-5.5", Owner: "microsoft-365", Tools: true, Reasoning: true, ContextWindow: 128000, MaxInputTokens: 96000, MaxOutputTokens: 16384},
	{ID: "gpt-5.5-reasoning", Owner: "microsoft-365", Tools: true, Reasoning: true, ContextWindow: 128000, MaxInputTokens: 96000, MaxOutputTokens: 16384},
	{ID: "gpt-5.6-sol", Owner: "microsoft-365", Tools: true, Reasoning: true, ContextWindow: 128000, MaxInputTokens: 96000, MaxOutputTokens: 16384},
	{ID: "gpt-5.6-reasoning", Owner: "microsoft-365", Tools: true, Reasoning: true, ContextWindow: 128000, MaxInputTokens: 96000, MaxOutputTokens: 16384},
	{ID: "claude-sonnet", Owner: "anthropic-via-microsoft-365", Tools: true, Reasoning: true, ContextWindow: 200000, MaxOutputTokens: 64000},
	{ID: "claude-sonnet-reasoning", Owner: "anthropic-via-microsoft-365", Tools: true, Reasoning: true, ContextWindow: 200000, MaxOutputTokens: 64000},
	// Legacy OpenAI desktop picker slugs — accepted as aliases but exposed
	// in /v1/models so Codex Desktop's cached catalog matches what we serve.
	{ID: "gpt-5.6-terra", Owner: "microsoft-365", Tools: true, Reasoning: true, ContextWindow: 128000, MaxInputTokens: 96000, MaxOutputTokens: 16384},
	{ID: "gpt-5.6-luna", Owner: "microsoft-365", Tools: true, Reasoning: true, ContextWindow: 128000, MaxInputTokens: 96000, MaxOutputTokens: 16384},
}

var gatewayModelAliases = map[string]string{
	// Canonical M365 bridge names
	"m365-copilot": "gpt-5.6-sol",
	"gpt-5.6":      "gpt-5.6-sol",
	"claude":       "claude-sonnet",
	"quick":        "gpt-5.4",
	"think-deeper": "gpt-5.6-reasoning",
	// OpenAI desktop picker legacy slugs — Codex Desktop caches its model
	// list from OpenAI servers and shows these regardless of provider.
	// Map them all to working M365 Copilot tones so every picker entry works.
	"gpt-5.6-terra":        "gpt-5.6-sol",
	"gpt-5.6-luna":         "gpt-5.6-reasoning",
	"gpt-5.4-quick":        "gpt-5.4",
	"gpt-5.4-mini":         "gpt-5.3",
	"gpt-5.3-think-deeper": "gpt-5.3",
	"gpt-reserve":          "gpt-5.5",
	"gpt-5.2-quick":        "gpt-5.2",
	"gpt-5.2-think-deeper": "gpt-5.2-reasoning",
	"gpt-5.3-quick":        "gpt-5.3",
	"gpt-5.5-quick":        "gpt-5.5",
}

func canonicalGatewayModel(model string) (string, bool) {
	wanted := strings.ToLower(strings.TrimSpace(model))
	if wanted == "" {
		wanted = "gpt-5.6-sol"
	}
	if canonical := gatewayModelAliases[wanted]; canonical != "" {
		wanted = canonical
	}
	for _, spec := range gatewayModels {
		if spec.ID == wanted {
			return wanted, true
		}
	}
	return wanted, false
}

func (m modelSpec) limits() modelLimits {
	maxOutput := m.MaxOutputTokens
	if maxOutput >= m.ContextWindow {
		maxOutput = m.ContextWindow / 8
	}
	maxInput := m.MaxInputTokens
	if maxInput <= 0 || maxInput+maxOutput > m.ContextWindow {
		maxInput = m.ContextWindow - maxOutput
	}
	return modelLimits{
		ContextWindow:   m.ContextWindow,
		MaxInputTokens:  maxInput,
		MaxOutputTokens: maxOutput,
	}
}

func modelLimitsFor(model string) modelLimits {
	wanted, _ := canonicalGatewayModel(model)
	for _, spec := range gatewayModels {
		if spec.ID == wanted {
			return spec.limits()
		}
	}
	return configuredModelLimits()
}

func positiveEnvInt(name string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err == nil && v > 0 {
		return v
	}
	return fallback
}
func configuredModelLimits() modelLimits {
	cfg := currentSettings()
	contextWindow := cfg.ContextWindow
	maxOutput := cfg.MaxOutputTokens
	if maxOutput >= contextWindow {
		maxOutput = contextWindow / 8
		if maxOutput < 1 {
			maxOutput = 1
		}
	}
	return modelLimits{ContextWindow: contextWindow, MaxInputTokens: contextWindow - maxOutput, MaxOutputTokens: maxOutput}
}
func normalizeReasoningEffort(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	if e == "" {
		return "", nil
	}
	switch e {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return e, nil
	case "max":
		return "xhigh", nil
	}
	return "", fmt.Errorf("unsupported reasoning effort %q; use none, minimal, low, medium, high, max, or xhigh", e)
}
func reasoningTone(model, effort string) (string, error) {
	e, err := normalizeReasoningEffort(effort)
	if err != nil {
		return "", err
	}
	base := modelTone(model)
	if strings.EqualFold(strings.TrimSpace(model), "gpt-5.3") && e != "" && e != "none" && e != "minimal" && e != "low" {
		return "", fmt.Errorf("model gpt-5.3 does not expose a reliable reasoning tone; use gpt-5.3 without reasoning effort")
	}
	// OpenAI defines medium as the default effort for gpt-5.6-sol. Microsoft
	// exposes that model family through the Gpt_5_6_Reasoning/Chat tones rather
	// than a literal Gpt_5_6_Sol tone, so preserve the public default here.
	if strings.EqualFold(strings.TrimSpace(model), "gpt-5.6-sol") && e == "" {
		return "Gpt_5_6_Reasoning", nil
	}
	// Explicit reasoning aliases are never silently downgraded by a generic client default.
	if strings.Contains(strings.ToLower(model), "reasoning") {
		return base, nil
	}
	if e == "" || e == "none" || e == "minimal" || e == "low" {
		return base, nil
	}
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "claude", "claude-sonnet":
		return "Claude_Sonnet_Reasoning", nil
	case "gpt-5.2":
		return "Gpt_5_2_Reasoning", nil
	case "gpt-5.3":
		return "Gpt_5_3_Reasoning", nil
	case "gpt-5.4":
		return "Gpt_5_4_Reasoning", nil
	case "gpt-5.5":
		return "Gpt_5_5_Reasoning", nil
	case "gpt-5.6", "gpt-5.6-sol":
		return "Gpt_5_6_Reasoning", nil
	default:
		return "Gpt_Reasoning", nil
	}
}
func modelCatalog() []map[string]any {
	out := make([]map[string]any, 0, len(gatewayModels))
	for _, m := range gatewayModels {
		l := m.limits()
		// Keep capability fields both at the top level and under capabilities:
		// different OpenAI-compatible clients inspect different locations.
		features := []string{"tools", "function_calling", "streaming", "vision"}
		if m.Reasoning {
			features = append(features, "reasoning")
		}
		modalities := []string{"text", "image"}
		caps := map[string]any{
			"chat_completions": true, "responses": true, "streaming": true,
			"tools": true, "reasoning": m.Reasoning, "reasoning_efforts": []string{"none", "minimal", "low", "medium", "high", "xhigh"},
			"reasoning_mode": map[bool]string{true: "gateway_tone_routing", false: "unsupported"}[m.Reasoning], "supports_tools": true, "tool_calls": true,
			"function_calling": true, "supports_function_calling": true, "supports_vision": true,
			"vision": true, "modalities": modalities, "input_modalities": modalities,
			"output_modalities": []string{"text"}, "supported_features": features,
			"backend": "microsoft-365", "compatibility_alias": true,
		}
		out = append(out, map[string]any{
			"id": m.ID, "object": "model", "owned_by": m.Owner,
			"context_window": l.ContextWindow, "max_input_tokens": l.MaxInputTokens, "max_output_tokens": l.MaxOutputTokens,
			"capabilities": caps, "supports_tools": true, "tool_calls": true,
			"function_calling": true, "supports_function_calling": true, "supports_vision": true,
			"vision": true, "modalities": modalities, "input_modalities": modalities,
			"output_modalities": []string{"text"}, "supported_features": features,
			"backend": "microsoft-365", "compatibility_alias": true,
		})
	}
	return out
}
