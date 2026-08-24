package web

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

const (
	policyRefusalTTL        = disengagedCooldown
	maxPolicyRefusalEntries = 2048
)

func policyRefusalKey(prompt string) [32]byte {
	return sha256.Sum256([]byte(strings.TrimSpace(prompt)))
}

func (s *Server) persistedPolicyRefusals() map[string]time.Time {
	now := time.Now()
	s.policyRefusalMu.Lock()
	defer s.policyRefusalMu.Unlock()
	out := make(map[string]time.Time)
	for key, until := range s.policyRefusals {
		if now.Before(until) {
			out[hex.EncodeToString(key[:])] = until
		} else {
			delete(s.policyRefusals, key)
		}
	}
	return out
}

func (s *Server) restorePolicyRefusals(state map[string]time.Time) {
	now := time.Now()
	restored := make(map[[32]byte]time.Time)
	for encoded, until := range state {
		if !now.Before(until) || len(restored) >= maxPolicyRefusalEntries {
			continue
		}
		decoded, err := hex.DecodeString(encoded)
		if err != nil || len(decoded) != sha256.Size {
			continue
		}
		var key [32]byte
		copy(key[:], decoded)
		restored[key] = until
	}
	s.policyRefusalMu.Lock()
	s.policyRefusals = restored
	s.policyRefusalMu.Unlock()
}

// rememberPolicyRefusal records only a digest: prompts, credentials, account
// identifiers and user content are never retained in this process-level guard.
func (s *Server) rememberPolicyRefusal(prompt string) {
	if strings.TrimSpace(prompt) == "" {
		return
	}
	now := time.Now()
	key := policyRefusalKey(prompt)
	s.policyRefusalMu.Lock()
	defer s.policyRefusalMu.Unlock()
	if s.policyRefusals == nil {
		s.policyRefusals = make(map[[32]byte]time.Time)
	}
	var oldestKey [32]byte
	var oldest time.Time
	for candidate, until := range s.policyRefusals {
		if !now.Before(until) {
			delete(s.policyRefusals, candidate)
			continue
		}
		if oldest.IsZero() || until.Before(oldest) {
			oldestKey, oldest = candidate, until
		}
	}
	if _, exists := s.policyRefusals[key]; !exists && len(s.policyRefusals) >= maxPolicyRefusalEntries && !oldest.IsZero() {
		delete(s.policyRefusals, oldestKey)
	}
	s.policyRefusals[key] = now.Add(policyRefusalTTL)
}

func (s *Server) recentPolicyRefusal(prompt string) bool {
	if strings.TrimSpace(prompt) == "" {
		return false
	}
	key := policyRefusalKey(prompt)
	s.policyRefusalMu.Lock()
	defer s.policyRefusalMu.Unlock()
	until, ok := s.policyRefusals[key]
	if !ok {
		return false
	}
	if !time.Now().Before(until) {
		delete(s.policyRefusals, key)
		return false
	}
	return true
}
