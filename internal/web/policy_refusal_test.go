package web

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPolicyRefusalCacheIsBoundedAndStoresNoPromptText(t *testing.T) {
	s := &Server{}
	for i := 0; i < maxPolicyRefusalEntries+100; i++ {
		s.rememberPolicyRefusal(fmt.Sprintf("sensitive prompt %d", i))
	}
	if len(s.policyRefusals) != maxPolicyRefusalEntries {
		t.Fatalf("refusal cache size=%d want=%d", len(s.policyRefusals), maxPolicyRefusalEntries)
	}
	if !s.recentPolicyRefusal("sensitive prompt 2147") {
		t.Fatal("newest refusal was evicted")
	}
	for key := range s.policyRefusals {
		if len(key) != 32 {
			t.Fatalf("cache key is not a SHA-256 digest: %T", key)
		}
	}
}

func TestPolicyRefusalCacheExpires(t *testing.T) {
	s := &Server{policyRefusals: make(map[[32]byte]time.Time)}
	key := policyRefusalKey("expired prompt")
	s.policyRefusals[key] = time.Now().Add(-time.Second)
	if s.recentPolicyRefusal("expired prompt") {
		t.Fatal("expired refusal remained active")
	}
	if len(s.policyRefusals) != 0 {
		t.Fatal("expired refusal was not removed")
	}
}

func TestPolicyRefusalDigestPersistenceSurvivesRestart(t *testing.T) {
	first := &Server{}
	first.rememberPolicyRefusal("do not fan this prompt across accounts")
	state := first.persistedPolicyRefusals()
	if len(state) != 1 {
		t.Fatalf("persisted refusal count=%d", len(state))
	}
	for encoded := range state {
		if strings.Contains(encoded, "do not fan") || len(encoded) != 64 {
			t.Fatalf("persistence exposed prompt text or invalid digest: %q", encoded)
		}
	}

	restarted := &Server{}
	restarted.restorePolicyRefusals(state)
	if !restarted.recentPolicyRefusal("do not fan this prompt across accounts") {
		t.Fatal("restart lost active policy-refusal guard")
	}
	if restarted.recentPolicyRefusal("different prompt") {
		t.Fatal("restart guard blocked unrelated prompt")
	}
}
