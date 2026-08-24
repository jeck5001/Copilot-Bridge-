package web

import (
	"strings"
	"testing"
)

func TestTrimContinuationOverlap(t *testing.T) {
	if got := trimContinuationOverlap("alpha beta gamma", "beta gamma delta"); got != " delta" {
		t.Fatalf("overlap trim=%q", got)
	}
	if got := trimContinuationOverlap("中文末尾", "末尾继续"); got != "继续" {
		t.Fatalf("unicode overlap trim=%q", got)
	}
	if got := trimContinuationOverlap("left", "different"); got != "different" {
		t.Fatalf("non-overlap changed=%q", got)
	}
}

func TestVisibleStreamResumePromptIsBoundedAndKeepsTail(t *testing.T) {
	prompt := visibleStreamResumePrompt(strings.Repeat("前", 20000) + "TAIL_MARKER")
	if !strings.Contains(prompt, "TAIL_MARKER") || strings.Count(prompt, "前") != 15989 {
		t.Fatalf("resume prompt tail was not rune-bounded: runes=%d", len([]rune(prompt)))
	}
}
