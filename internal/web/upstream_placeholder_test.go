package web

import (
	"strings"
	"testing"

	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

func TestUpstreamThrottlePlaceholderIsNotReusableContent(t *testing.T) {
	text := "We're temporarily unable to respond to this volume of requests. Please try again later."
	if !isUpstreamThrottlePlaceholderText(text) {
		t.Fatal("known overload placeholder was not recognized")
	}
	if !quotaFailureWithoutContent(chathub.Result{Text: text, FullText: text}) {
		t.Fatal("overload placeholder was treated as a successful answer")
	}
	if isUpstreamThrottlePlaceholderText("The service was overloaded earlier, but deployment is complete.") {
		t.Fatal("ordinary model prose was classified as an overload placeholder")
	}
}

func TestStreamPlaceholderGuardHoldsSplitOverloadAndReleasesNormalText(t *testing.T) {
	var guard streamPlaceholderGuard
	for _, part := range []string{"We're temporarily ", "unable to respond to this volume ", "of requests. Please try again later."} {
		if released, held := guard.Feed(part); released != "" || !held {
			t.Fatalf("placeholder fragment leaked: released=%q held=%v", released, held)
		}
	}
	if leaked := guard.Finish(true); leaked != "" {
		t.Fatalf("placeholder leaked at finish: %q", leaked)
	}

	guard.Reset()
	var normal strings.Builder
	for _, part := range []string{"We", "lcome to the result."} {
		if released, _ := guard.Feed(part); released != "" {
			normal.WriteString(released)
		}
	}
	normal.WriteString(guard.Finish(false))
	if normal.String() != "Welcome to the result." {
		t.Fatalf("normal stream was corrupted: %q", normal.String())
	}
}
