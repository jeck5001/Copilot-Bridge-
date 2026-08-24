package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// stableSessionKey converts client-owned conversation identifiers into an
// opaque local key. prompt_cache_key is intentionally excluded: the official
// Responses contract defines it only as a cache bucket, not a conversation
// identity. Treating it as a thread silently mixed unrelated requests that
// happened to share a cache key.
func (r responsesRequest) stableSessionKey() string {
	if r.NewConversation {
		return ""
	}
	if previous := strings.TrimSpace(r.PreviousResponseID); previous != "" {
		return responseIDSessionKey(previous)
	}
	candidates := []string{}
	if r.ClientMetadata != nil {
		for _, key := range []string{"thread_id", "session_id", "root_turn_id"} {
			if value, ok := r.ClientMetadata[key].(string); ok {
				candidates = append(candidates, value)
			}
		}
	}
	switch value := r.Conversation.(type) {
	case string:
		candidates = append(candidates, value)
	case map[string]any:
		if id, ok := value["id"].(string); ok {
			candidates = append(candidates, id)
		}
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		h := sha256.Sum256([]byte("m365-responses-session\x00" + candidate))
		return "responses_" + hex.EncodeToString(h[:16])
	}
	return ""
}

func responseIDSessionKey(responseID string) string {
	h := sha256.Sum256([]byte("m365-response-id\x00" + strings.TrimSpace(responseID)))
	return "response_" + hex.EncodeToString(h[:16])
}

// lockStableResponseSession serializes requests that deliberately share a
// stable client thread key. previous_response_id branches remain concurrent;
// only a mutable stable head needs single-writer semantics.
func (s *Server) lockStableResponseSession(key string) (func(), bool) {
	if key == "" {
		return func() {}, true
	}
	s.responseSessionMu.Lock()
	if s.responseSessionLocks == nil {
		s.responseSessionLocks = map[string]*keyedResponseLock{}
	}
	entry := s.responseSessionLocks[key]
	if entry == nil {
		entry = &keyedResponseLock{}
		s.responseSessionLocks[key] = entry
	}
	entry.refs++
	s.responseSessionMu.Unlock()
	// A second writer to the same mutable stable head must not wait behind a
	// long-running model call.  Waiting here looks like a dead request to agent
	// clients and can outlive their own retry deadline.  Fail fast so the client
	// can retry after the in-flight response reaches a terminal state.
	if !entry.mu.TryLock() {
		s.responseSessionMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(s.responseSessionLocks, key)
		}
		s.responseSessionMu.Unlock()
		return func() {}, false
	}
	return func() {
		entry.mu.Unlock()
		s.responseSessionMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(s.responseSessionLocks, key)
		}
		s.responseSessionMu.Unlock()
	}, true
}

func (s *Server) aliasResponseSession(r *http.Request, sourceRawKey, responseID string) error {
	source := scopedSessionKey(r, sourceRawKey)
	alias := scopedSessionKey(r, responseIDSessionKey(responseID))
	if source == "" || alias == "" {
		return fmt.Errorf("response alias source or target missing")
	}
	// A first Responses request without prompt_cache_key or client metadata is
	// rooted directly at the response ID that will be returned. In that case
	// source and alias intentionally resolve to the same credential-scoped key:
	// portable history is already persisted there and no copy is necessary.
	if source == alias {
		if _, ok := s.sessions.get(source); !ok {
			return fmt.Errorf("response alias source %s missing", source)
		}
		return nil
	}
	conv, immutableParent, ok := s.sessions.aliasSource(source)
	if !ok {
		return fmt.Errorf("response alias source %s missing", source)
	}
	conv.ID = alias
	if conv.AliasGroup == "" {
		conv.AliasGroup = source
	}
	conv.ResponseAlias = true
	if immutableParent != "" {
		conv.PortableMessages = nil
		conv.HistoryParent = immutableParent
		conv.HistoryDrop = 0
		conv.PortableDelta = nil
	} else {
		// A legacy/full stable root is mutable and therefore cannot be an
		// immutable parent. Keep one full snapshot; subsequent aliases can link
		// compactly to this response alias.
		conv.HistoryParent = ""
		conv.HistoryDrop = 0
		conv.PortableDelta = nil
	}
	if _, err := s.sessions.upsert(conv); err != nil {
		return fmt.Errorf("persist response alias: %w", err)
	}
	return nil
}

func (s *Server) persistResponsePortableHistory(r *http.Request, sourceRawKey string, requestMessages []oaiMsg, assistant oaiMsg) error {
	source := scopedSessionKey(r, sourceRawKey)
	if source == "" {
		return fmt.Errorf("portable response session key missing")
	}
	existing, ok := s.sessions.get(source)
	if !ok {
		return fmt.Errorf("portable response source %s missing", source)
	}
	history := mergePortableMessages(existing.PortableMessages, requestMessages)
	history = append(history, assistant)
	_, err := s.sessions.updatePortable(source, history)
	return err
}

// prepareResponseTarget validates the immutable source and reserves a private
// response key logically. It deliberately does not rewrite the full session
// store before response.created; the producer persists the target binding and
// commitResponse atomically materializes any still-missing target.
func (s *Server) prepareResponseTarget(r *http.Request, sourceRawKey, targetRawKey string, requireSource bool) (func(bool), error) {
	source := scopedSessionKey(r, sourceRawKey)
	target := scopedSessionKey(r, targetRawKey)
	if target == "" {
		return nil, fmt.Errorf("response target missing")
	}
	if source == target {
		_, existed := s.sessions.get(target)
		return func(committed bool) {
			if !committed && !existed {
				_, _ = s.sessions.delete(target)
			}
		}, nil
	}
	unlockStable := func() {}
	if !requireSource {
		var locked bool
		unlockStable, locked = s.lockStableResponseSession(source)
		if !locked {
			return nil, fmt.Errorf("stable response session already active; retry after the in-flight response completes")
		}
	}
	_, ok := s.sessions.pin(source)
	if !ok && requireSource {
		unlockStable()
		return nil, fmt.Errorf("previous response session not found")
	}
	return func(committed bool) {
		if ok {
			s.sessions.unpin(source)
		}
		if !committed {
			if _, err := s.sessions.delete(target); err != nil {
				fmt.Printf("[sessions] discard incomplete response target %s: %v\n", target, err)
			}
		}
		unlockStable()
	}, nil
}

func (s *Server) commitResponsePortableHistory(r *http.Request, sourceRawKey, targetRawKey string, requestMessages []oaiMsg, assistant oaiMsg, advanceSource bool) error {
	source := scopedSessionKey(r, sourceRawKey)
	target := scopedSessionKey(r, targetRawKey)
	effectiveAdvance := advanceSource
	if advanceSource && source != target {
		if targetState, ok := s.sessions.get(target); ok && targetState.AccountID != "" && s.tokens != nil {
			if _, authoritative := s.accountRouteVersion(targetState.AccountID); !authoritative {
				effectiveAdvance = false
				fmt.Printf("[sessions] response target %s belongs to inactive account; stable head not advanced\n", target)
			}
		}
	}
	if _, err := s.sessions.commitResponse(source, target, requestMessages, assistant, effectiveAdvance); err != nil {
		return fmt.Errorf("commit response continuation state: %w", err)
	}
	return nil
}

func promptTitle(messages []oaiMsg) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			continue
		}
		text := strings.TrimSpace(contentToString(messages[i].Content))
		if text == "" {
			continue
		}
		return boundedSessionTitle(text)
	}
	return fmt.Sprintf("conversation-%d", len(messages))
}

func boundedSessionTitle(text string) string {
	return compactToolResult(strings.TrimSpace(text), 240)
}

// scopedSessionKey prevents two API keys that happen to reuse a client-side
// session identifier from sharing an upstream Microsoft conversation.
func scopedSessionKey(r *http.Request, key string) string {
	if key == "" {
		return ""
	}
	credential := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if credential == "" {
		credential = strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(credential), "bearer ") {
			credential = strings.TrimSpace(credential[7:])
		}
	}
	h := sha256.Sum256([]byte("m365-session-scope\x00" + credential + "\x00" + key))
	return "session_" + hex.EncodeToString(h[:16])
}
