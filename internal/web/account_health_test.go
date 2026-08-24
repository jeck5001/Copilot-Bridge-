package web

import (
	"errors"
	"testing"
	"time"
)

func TestAccountHealthTransientCooldown(t *testing.T) {
	h := newAccountHealth()
	h.MarkTransient("a")
	if h.Available("a") {
		t.Fatal("transiently failing account should be unavailable")
	}
	h.mu.Lock()
	h.transient["a"] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if !h.Available("a") {
		t.Fatal("expired transient cooldown should become available")
	}
}

func TestIsTransientUpstream(t *testing.T) {
	for _, msg := range []string{
		"ws read before completion: websocket: close 1006 (abnormal closure): unexpected EOF",
		"ws dial: i/o timeout",
		"chathub returned no content",
	} {
		if !IsTransientUpstream(errors.New(msg)) {
			t.Fatalf("expected transient classification for %q", msg)
		}
	}
	if IsTransientUpstream(errors.New("invalid request payload")) {
		t.Fatal("invalid request must not be treated as transient")
	}
}
