package web

import (
	"testing"

	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

func TestQuotaBoundaryKeepsCompletedBufferedAnswer(t *testing.T) {
	throttling := map[string]any{
		"metering": map[string]any{
			"CostQuota": map[string]any{"remainingAllowance": float64(0)},
		},
	}
	complete := chathub.Result{Text: "complete answer", Throttling: throttling}
	if quotaFailureWithoutContent(complete) {
		t.Fatal("complete answer was discarded at the quota boundary")
	}

	empty := chathub.Result{Throttling: throttling}
	if !quotaFailureWithoutContent(empty) {
		t.Fatal("empty exhausted response was not classified as a quota failure")
	}
}
