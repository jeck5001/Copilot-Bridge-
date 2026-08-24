package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type conversation struct {
	ID               string   `json:"id"`
	AccountID        string   `json:"accountId"`
	ConversationID   string   `json:"conversationId"`
	SessionID        string   `json:"sessionId"`
	Title            string   `json:"title,omitempty"`
	PortableMessages []oaiMsg `json:"portableMessages,omitempty"`
	// New stores keep one materialized root and encode immutable response
	// snapshots as parent+drop+delta. Stable thread heads reference their latest
	// immutable response. Legacy portableMessages remains the root/wire format.
	HistoryParent   string    `json:"historyParent,omitempty"`
	HistoryDrop     int       `json:"historyDrop,omitempty"`
	PortableDelta   []oaiMsg  `json:"portableDelta,omitempty"`
	AliasGroup      string    `json:"aliasGroup,omitempty"`
	ResponseAlias   bool      `json:"responseAlias,omitempty"`
	RouteGeneration uint64    `json:"routeGeneration,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type sessionStore struct {
	mu   sync.Mutex
	path string
	data map[string]conversation
	pins map[string]int
}

const (
	maxSessionKeyLength = 256
	maxStoredSessions   = 5000
	sessionTTL          = 30 * 24 * time.Hour
	// A response ID is an immutable snapshot of one logical Responses chain.
	// Retaining a small recent window supports previous_response_id without the
	// old O(turns) alias growth eventually crowding out the stable thread key.
	maxResponseAliasesPerSession = 128
	// Portable history is already bounded per conversation. This second bound
	// limits the complete indented sessions.json file, including alias copies.
	maxSessionStoreBytes = 16 << 20
)

func openSessionStore() (*sessionStore, error) {
	path := os.Getenv("M365_SESSION_CACHE")
	if path == "" {
		path = filepath.Join(os.TempDir(), "m365-gateway-sessions.json")
	}
	s := &sessionStore{path: path, data: map[string]conversation{}, pins: map[string]int{}}
	loaded := false
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, fmt.Errorf("decode session store: %w", err)
		}
		loaded = true
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read session store: %w", err)
	}
	// Legacy stores predate AliasGroup/ResponseAlias. Their response aliases are
	// opaque credential-scoped hashes, so recover chains by the account-owned
	// upstream triple they copied. Keep the earliest deterministic member as the
	// stable root and classify the rest as aliases; blank triples are never
	// grouped because unrelated fresh conversations would otherwise collapse.
	legacyGroups := map[string][]string{}
	for key, value := range s.data {
		if value.AliasGroup != "" || value.AccountID == "" || value.ConversationID == "" || value.SessionID == "" {
			continue
		}
		groupKey := value.AccountID + "\x00" + value.ConversationID + "\x00" + value.SessionID
		legacyGroups[groupKey] = append(legacyGroups[groupKey], key)
	}
	for _, keys := range legacyGroups {
		sort.Slice(keys, func(i, j int) bool {
			left, right := s.data[keys[i]], s.data[keys[j]]
			if !left.CreatedAt.Equal(right.CreatedAt) {
				return left.CreatedAt.Before(right.CreatedAt)
			}
			if !left.UpdatedAt.Equal(right.UpdatedAt) {
				return left.UpdatedAt.Before(right.UpdatedAt)
			}
			return keys[i] < keys[j]
		})
		root := keys[0]
		for _, key := range keys {
			value := s.data[key]
			value.AliasGroup = root
			value.ResponseAlias = key != root
			s.data[key] = value
		}
	}
	for key, value := range s.data {
		value.ID = key
		if value.AliasGroup == "" {
			value.AliasGroup = value.ID
		}
		value.PortableMessages = boundedPortableMessages(value.PortableMessages)
		value.PortableDelta = sanitizePortableDelta(value.PortableDelta)
		s.data[key] = value
	}
	s.compactLegacyPortableCopiesLocked()
	for key := range s.data {
		if _, err := s.materializeHistoryLocked(key, map[string]bool{}); err != nil {
			return nil, fmt.Errorf("decode portable history for %s: %w", key, err)
		}
	}
	encoded := s.pruneLocked(time.Now().UTC())
	if loaded {
		if err := atomicWriteFile(s.path, encoded, 0o600); err != nil {
			return nil, fmt.Errorf("persist migrated session store: %w", err)
		}
	}
	return s, nil
}

// compactLegacyPortableCopiesLocked migrates stores written by the old alias
// copier. One immutable response alias remains the materialized base; every
// other member is represented as a transition from it. This preserves every
// old response snapshot exactly while removing the common O(aliases*history)
// duplication on the first restart after upgrade.
func (s *sessionStore) compactLegacyPortableCopiesLocked() {
	groups := map[string][]string{}
	for key, value := range s.data {
		group := firstNonEmpty(value.AliasGroup, value.ID, key)
		groups[group] = append(groups[group], key)
	}
	for _, keys := range groups {
		if len(keys) < 2 {
			continue
		}
		structured := false
		for _, key := range keys {
			value := s.data[key]
			if value.HistoryParent != "" || value.HistoryDrop != 0 || len(value.PortableDelta) > 0 {
				structured = true
				break
			}
		}
		if structured {
			continue
		}
		sort.Slice(keys, func(i, j int) bool {
			left, right := s.data[keys[i]], s.data[keys[j]]
			if left.ResponseAlias != right.ResponseAlias {
				return left.ResponseAlias
			}
			if !left.CreatedAt.Equal(right.CreatedAt) {
				return left.CreatedAt.Before(right.CreatedAt)
			}
			return keys[i] < keys[j]
		})
		baseID := ""
		for _, key := range keys {
			if s.data[key].ResponseAlias {
				baseID = key
				break
			}
		}
		if baseID == "" {
			continue
		}
		baseHistory := boundedPortableMessages(s.data[baseID].PortableMessages)
		for _, key := range keys {
			if key == baseID {
				continue
			}
			value := s.data[key]
			drop, delta := portableTransition(baseHistory, value.PortableMessages)
			value.PortableMessages = nil
			value.HistoryParent = baseID
			value.HistoryDrop = drop
			value.PortableDelta = delta
			s.data[key] = value
		}
	}
}

func (s *sessionStore) saveDataLocked(data map[string]conversation) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.path, b, 0o600)
}

func cloneConversations(src map[string]conversation) map[string]conversation {
	out := make(map[string]conversation, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func (s *sessionStore) needsStructuralPruneLocked(now time.Time) bool {
	if len(s.data) > maxStoredSessions {
		return true
	}
	aliases := map[string]int{}
	for key, value := range s.data {
		if strings.TrimSpace(key) == "" || len(key) > maxSessionKeyLength || value.UpdatedAt.IsZero() || now.Sub(value.UpdatedAt) > sessionTTL {
			return true
		}
		if value.ResponseAlias {
			aliases[firstNonEmpty(value.AliasGroup, value.ID, key)]++
			if aliases[firstNonEmpty(value.AliasGroup, value.ID, key)] > maxResponseAliasesPerSession {
				return true
			}
		}
	}
	return false
}

// preparePersistLocked avoids the old O(n) map clone and duplicate full JSON
// marshal on ordinary writes. A full transactional snapshot is allocated only
// when pruning can mutate unrelated keys; otherwise callers need roll back just
// the entries they changed.
func (s *sessionStore) preparePersistLocked(now time.Time) ([]byte, map[string]conversation) {
	encoded, _ := json.MarshalIndent(s.data, "", "  ")
	if !s.needsStructuralPruneLocked(now) && len(encoded) <= maxSessionStoreBytes {
		return encoded, nil
	}
	beforePrune := cloneConversations(s.data)
	return s.pruneLocked(now), beforePrune
}

func restoreConversation(data map[string]conversation, id string, value conversation, existed bool) {
	if existed {
		data[id] = value
	} else {
		delete(data, id)
	}
}

func (s *sessionStore) pinnedDependencyLocked(id string) bool {
	if s.pins[id] > 0 {
		return true
	}
	for pinned, count := range s.pins {
		if count <= 0 {
			continue
		}
		seen := map[string]bool{}
		current := pinned
		for current != "" && !seen[current] {
			if current == id {
				return true
			}
			seen[current] = true
			value, ok := s.data[current]
			if !ok {
				break
			}
			current = value.HistoryParent
		}
	}
	return false
}

// deleteWithRebaseLocked preserves every surviving descendant before removing
// an immutable parent. Direct children become materialized roots; descendants
// below them keep their compact deltas unchanged.
func (s *sessionStore) deleteWithRebaseLocked(id string) error {
	if _, ok := s.data[id]; !ok {
		return nil
	}
	for childID, child := range s.data {
		if child.HistoryParent != id {
			continue
		}
		history, err := s.materializeHistoryLocked(childID, map[string]bool{})
		if err != nil {
			return err
		}
		child.PortableMessages = history
		child.HistoryParent = ""
		child.HistoryDrop = 0
		child.PortableDelta = nil
		s.data[childID] = child
	}
	delete(s.data, id)
	return nil
}

func hasPortableState(value conversation) bool {
	return len(value.PortableMessages) > 0 || value.HistoryParent != "" || len(value.PortableDelta) > 0 || value.HistoryDrop > 0
}

func sanitizePortableDelta(messages []oaiMsg) []oaiMsg {
	if len(messages) == 0 {
		return nil
	}
	out := make([]oaiMsg, 0, len(messages))
	for _, message := range messages {
		if clean, ok := sanitizePortableMessage(message); ok {
			out = append(out, clean)
		}
	}
	return clonePortableMessages(out)
}

func (s *sessionStore) materializeHistoryLocked(id string, visiting map[string]bool) ([]oaiMsg, error) {
	value, ok := s.data[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	if visiting[id] {
		return nil, fmt.Errorf("cyclic portable history at %s", id)
	}
	visiting[id] = true
	defer delete(visiting, id)
	if value.HistoryParent == "" {
		return boundedPortableMessages(value.PortableMessages), nil
	}
	parent, err := s.materializeHistoryLocked(value.HistoryParent, visiting)
	if err != nil {
		return nil, fmt.Errorf("materialize parent %s for %s: %w", value.HistoryParent, id, err)
	}
	if value.HistoryDrop < 0 || value.HistoryDrop > len(parent) {
		return nil, fmt.Errorf("invalid portable history drop %d for %s", value.HistoryDrop, id)
	}
	history := append(clonePortableMessages(parent[value.HistoryDrop:]), clonePortableMessages(value.PortableDelta)...)
	return boundedPortableMessages(history), nil
}

func (s *sessionStore) materializedConversationLocked(id string) (conversation, bool) {
	value, ok := s.data[id]
	if !ok {
		return conversation{}, false
	}
	history, err := s.materializeHistoryLocked(id, map[string]bool{})
	if err != nil {
		return conversation{}, false
	}
	value.PortableMessages = history
	return value, true
}

func (s *sessionStore) pruneLocked(now time.Time) []byte {
	for k, v := range s.data {
		invalid := strings.TrimSpace(k) == "" || len(k) > maxSessionKeyLength || v.UpdatedAt.IsZero()
		expired := now.Sub(v.UpdatedAt) > sessionTTL
		if (invalid || expired) && !s.pinnedDependencyLocked(k) {
			_ = s.deleteWithRebaseLocked(k)
		}
	}
	aliasesByGroup := map[string][]conversation{}
	for _, value := range s.data {
		if !value.ResponseAlias {
			continue
		}
		group := value.AliasGroup
		if group == "" {
			group = value.ID
		}
		aliasesByGroup[group] = append(aliasesByGroup[group], value)
	}
	for _, aliases := range aliasesByGroup {
		if len(aliases) <= maxResponseAliasesPerSession {
			continue
		}
		sort.Slice(aliases, func(i, j int) bool { return aliases[i].UpdatedAt.After(aliases[j].UpdatedAt) })
		for _, value := range aliases[maxResponseAliasesPerSession:] {
			if !s.pinnedDependencyLocked(value.ID) {
				_ = s.deleteWithRebaseLocked(value.ID)
			}
		}
	}
	if len(s.data) <= maxStoredSessions {
		return s.pruneSerializedBytesLocked()
	}
	items := make([]conversation, 0, len(s.data))
	for _, v := range s.data {
		items = append(items, v)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ResponseAlias != items[j].ResponseAlias {
			return items[i].ResponseAlias
		}
		return items[i].UpdatedAt.Before(items[j].UpdatedAt)
	})
	for i := 0; len(s.data) > maxStoredSessions && i < len(items); i++ {
		if !s.pinnedDependencyLocked(items[i].ID) {
			_ = s.deleteWithRebaseLocked(items[i].ID)
		}
	}
	return s.pruneSerializedBytesLocked()
}

func (s *sessionStore) pruneSerializedBytesLocked() []byte {
	encoded, _ := json.MarshalIndent(s.data, "", "  ")
	if len(encoded) <= maxSessionStoreBytes {
		return encoded
	}
	items := make([]conversation, 0, len(s.data))
	for _, value := range s.data {
		items = append(items, value)
	}
	// Alias snapshots are always expendable before stable thread keys. Stable
	// keys are considered only if they alone exceed the hard file-size bound.
	sort.Slice(items, func(i, j int) bool {
		if items[i].ResponseAlias != items[j].ResponseAlias {
			return items[i].ResponseAlias
		}
		return items[i].UpdatedAt.Before(items[j].UpdatedAt)
	})
	remainingBytes := len(encoded)
	for _, value := range items {
		if s.pinnedDependencyLocked(value.ID) {
			continue
		}
		entry, _ := json.MarshalIndent(map[string]conversation{value.ID: value}, "", "  ")
		if err := s.deleteWithRebaseLocked(value.ID); err != nil {
			continue
		}
		// The two map braces are shared in the complete file. This is an
		// intentionally conservative approximation; verify exactly below.
		remainingBytes -= len(entry) - 2
		if remainingBytes <= maxSessionStoreBytes {
			encoded, _ = json.MarshalIndent(s.data, "", "  ")
			if len(encoded) <= maxSessionStoreBytes {
				return encoded
			}
			remainingBytes = len(encoded)
		}
		if len(s.data) == 0 {
			return []byte("{}")
		}
	}
	encoded, _ = json.MarshalIndent(s.data, "", "  ")
	return encoded
}

// pin prevents an immutable previous_response_id snapshot from being evicted
// while a child response is being generated. The pin is process-local and is
// always released by prepareResponseTarget's completion closure.
func (s *sessionStore) pin(id string) (conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.materializedConversationLocked(id)
	if !ok {
		return conversation{}, false
	}
	if s.pins == nil {
		s.pins = map[string]int{}
	}
	s.pins[id]++
	return v, true
}

func (s *sessionStore) unpin(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pins[id] <= 1 {
		delete(s.pins, id)
		return
	}
	s.pins[id]--
}

func (s *sessionStore) list() []conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]conversation, 0, len(s.data))
	for key := range s.data {
		if v, ok := s.materializedConversationLocked(key); ok {
			out = append(out, v)
		}
	}
	return out
}

func (s *sessionStore) get(id string) (conversation, bool) {
	if strings.TrimSpace(id) == "" || len(id) > maxSessionKeyLength {
		return conversation{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.materializedConversationLocked(id)
}

func (s *sessionStore) aliasSource(id string) (conversation, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	materialized, ok := s.materializedConversationLocked(id)
	if !ok {
		return conversation{}, "", false
	}
	raw := s.data[id]
	parent := ""
	if raw.ResponseAlias {
		parent = id
	} else if raw.HistoryParent != "" && raw.HistoryDrop == 0 && len(raw.PortableDelta) == 0 && len(raw.PortableMessages) == 0 {
		parent = raw.HistoryParent
	}
	return materialized, parent, true
}

func (s *sessionStore) maxRouteGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var maximum uint64
	for _, value := range s.data {
		if value.RouteGeneration > maximum {
			maximum = value.RouteGeneration
		}
	}
	return maximum
}

func (s *sessionStore) upsert(v conversation) (conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertLocked(v)
}

// upsertBinding prevents a draining request from an older account generation
// from overwriting the binding already committed by the new active account.
// Different sessions remain fully concurrent; only stale writes to the same
// key are discarded.
func (s *sessionStore) upsertBinding(v conversation, authoritative bool) (conversation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.data[v.ID]; ok {
		if current.RouteGeneration > v.RouteGeneration ||
			(current.RouteGeneration == v.RouteGeneration && current.AccountID != "" && current.AccountID != v.AccountID && !authoritative) {
			return current, false, nil
		}
	}
	stored, err := s.upsertLocked(v)
	return stored, err == nil, err
}

func (s *sessionStore) upsertLocked(v conversation) (conversation, error) {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	if len(v.ID) > maxSessionKeyLength {
		return conversation{}, fmt.Errorf("session key exceeds %d bytes", maxSessionKeyLength)
	}
	now := time.Now().UTC()
	prev, existed := s.data[v.ID]
	if v.AliasGroup == "" {
		if existed && prev.AliasGroup != "" {
			v.AliasGroup = prev.AliasGroup
		} else {
			v.AliasGroup = v.ID
		}
	}
	if !hasPortableState(v) && existed && hasPortableState(prev) {
		v.PortableMessages = clonePortableMessages(prev.PortableMessages)
		v.HistoryParent = prev.HistoryParent
		v.HistoryDrop = prev.HistoryDrop
		v.PortableDelta = clonePortableMessages(prev.PortableDelta)
	} else if v.HistoryParent != "" {
		v.PortableMessages = nil
		v.PortableDelta = sanitizePortableDelta(v.PortableDelta)
	} else {
		v.PortableMessages = boundedPortableMessages(v.PortableMessages)
		v.HistoryDrop = 0
		v.PortableDelta = nil
	}
	if v.CreatedAt.IsZero() {
		if existed && !prev.CreatedAt.IsZero() {
			v.CreatedAt = prev.CreatedAt
		} else {
			v.CreatedAt = now
		}
	}
	v.UpdatedAt = now
	v.Title = boundedSessionTitle(v.Title)
	s.data[v.ID] = v
	encoded, beforePrune := s.preparePersistLocked(now)
	if err := atomicWriteFile(s.path, encoded, 0o600); err != nil {
		if beforePrune != nil {
			s.data = beforePrune
		}
		restoreConversation(s.data, v.ID, prev, existed)
		return conversation{}, err
	}
	stored, _ := s.materializedConversationLocked(v.ID)
	return stored, nil
}

func (s *sessionStore) updatePortable(id string, messages []oaiMsg) (conversation, error) {
	if strings.TrimSpace(id) == "" || len(id) > maxSessionKeyLength {
		return conversation{}, fmt.Errorf("invalid session key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, ok := s.data[id]
	if !ok {
		return conversation{}, os.ErrNotExist
	}
	next := previous
	next.PortableMessages = boundedPortableMessages(messages)
	next.HistoryParent = ""
	next.HistoryDrop = 0
	next.PortableDelta = nil
	next.UpdatedAt = time.Now().UTC()
	s.data[id] = next
	encoded, beforePrune := s.preparePersistLocked(next.UpdatedAt)
	if err := atomicWriteFile(s.path, encoded, 0o600); err != nil {
		if beforePrune != nil {
			s.data = beforePrune
		}
		s.data[id] = previous
		return conversation{}, err
	}
	return next, nil
}

// commitResponse atomically appends the completed turn to the immutable target
// response snapshot and, when requested, advances a separate stable thread
// head. A single atomic file replacement prevents the head and response ID from
// diverging after disk-full, permission, or rename failures.
func (s *sessionStore) commitResponse(sourceID, targetID string, requestMessages []oaiMsg, assistant oaiMsg, advanceSource bool) (conversation, error) {
	if strings.TrimSpace(targetID) == "" || len(targetID) > maxSessionKeyLength {
		return conversation{}, fmt.Errorf("invalid response session key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	source, sourceExists := s.data[sourceID]
	previousSource := source
	if !advanceSource && sourceID != "" && sourceID != targetID && !sourceExists {
		return conversation{}, os.ErrNotExist
	}
	target, ok := s.data[targetID]
	previousTarget, targetExisted := target, ok
	if !ok {
		if sourceExists {
			target = source
			target.ID = targetID
			target.ResponseAlias = targetID != sourceID
			if target.AliasGroup == "" {
				target.AliasGroup = firstNonEmpty(sourceID, targetID)
			}
		} else if sourceID == targetID {
			target = conversation{ID: targetID, AliasGroup: targetID}
		} else {
			return conversation{}, os.ErrNotExist
		}
	}
	// The producer may already have materialized targetID to persist its fresh
	// upstream binding. Build the immutable history from the source snapshot,
	// then encode only a transition from an immutable parent. A mutable stable
	// head is never used as a parent because advancing it would rewrite history
	// observed through an older previous_response_id.
	var sourceHistory []oaiMsg
	if sourceExists {
		var err error
		sourceHistory, err = s.materializeHistoryLocked(sourceID, map[string]bool{})
		if err != nil {
			return conversation{}, err
		}
	}
	history := sourceHistory
	if ok {
		var err error
		targetHistory, materializeErr := s.materializeHistoryLocked(targetID, map[string]bool{})
		err = materializeErr
		if err != nil {
			return conversation{}, err
		}
		if sourceID == targetID {
			history = targetHistory
		} else {
			history = mergePortableMessages(history, targetHistory)
		}
	}
	history = mergePortableMessages(history, requestMessages)
	history = append(history, assistant)
	history = boundedPortableMessages(history)
	target.UpdatedAt = time.Now().UTC()
	if targetID != sourceID {
		target.ResponseAlias = true
		if sourceExists {
			target.AliasGroup = firstNonEmpty(source.AliasGroup, source.ID, sourceID)
		} else {
			target.AliasGroup = sourceID
		}
	} else if target.AliasGroup == "" {
		target.AliasGroup = targetID
	}
	parentID := ""
	if sourceExists && sourceID != targetID {
		switch {
		case source.ResponseAlias:
			parentID = sourceID
		case source.HistoryParent != "" && source.HistoryDrop == 0 && len(source.PortableDelta) == 0 && len(source.PortableMessages) == 0:
			// Stable heads are lightweight references to their current immutable
			// response; link the new response to that immutable node, not the
			// mutable stable key itself.
			parentID = source.HistoryParent
		}
	}
	if parentID == "" {
		target.PortableMessages = history
		target.HistoryParent = ""
		target.HistoryDrop = 0
		target.PortableDelta = nil
	} else {
		parentHistory, err := s.materializeHistoryLocked(parentID, map[string]bool{})
		if err != nil {
			return conversation{}, err
		}
		drop, delta := portableTransition(parentHistory, history)
		target.PortableMessages = nil
		target.HistoryParent = parentID
		target.HistoryDrop = drop
		target.PortableDelta = delta
	}
	s.data[targetID] = target
	if advanceSource && sourceID != "" && sourceID != targetID {
		head := target
		head.ID = sourceID
		head.AliasGroup = sourceID
		head.ResponseAlias = false
		head.PortableMessages = nil
		head.HistoryParent = targetID
		head.HistoryDrop = 0
		head.PortableDelta = nil
		if previous, exists := s.data[sourceID]; exists && !previous.CreatedAt.IsZero() {
			head.CreatedAt = previous.CreatedAt
		}
		s.data[sourceID] = head
	}
	encoded, beforePrune := s.preparePersistLocked(target.UpdatedAt)
	if err := atomicWriteFile(s.path, encoded, 0o600); err != nil {
		if beforePrune != nil {
			s.data = beforePrune
		}
		restoreConversation(s.data, targetID, previousTarget, targetExisted)
		if sourceID != targetID {
			restoreConversation(s.data, sourceID, previousSource, sourceExists)
		}
		return conversation{}, err
	}
	stored, _ := s.materializedConversationLocked(targetID)
	return stored, nil
}

func (s *sessionStore) delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[id]
	if !ok {
		return false, nil
	}
	previousData := cloneConversations(s.data)
	if err := s.deleteWithRebaseLocked(id); err != nil {
		return false, err
	}
	if err := s.saveDataLocked(s.data); err != nil {
		s.data = previousData
		return false, err
	}
	return true, nil
}

// deleteByAccount removes every cached conversation bound to the given account
// ID. Called when an account is deleted so stale session_key bindings don't keep
// returning 400 and don't pin a chat to a dead account (which would otherwise
// disable round-robin failover for that session).
func (s *sessionStore) deleteByAccount(accountID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	previousData := cloneConversations(s.data)
	removeIDs := map[string]bool{}
	for k, v := range s.data {
		if v.AccountID == accountID {
			removeIDs[k] = true
			removed++
		}
	}
	if removed > 0 {
		// Rebase only surviving children. Members deleted in the same transaction
		// need no materialized replacement.
		for childID, child := range s.data {
			if removeIDs[childID] || !removeIDs[child.HistoryParent] {
				continue
			}
			history, err := s.materializeHistoryLocked(childID, map[string]bool{})
			if err != nil {
				s.data = previousData
				return 0, err
			}
			child.PortableMessages = history
			child.HistoryParent = ""
			child.HistoryDrop = 0
			child.PortableDelta = nil
			s.data[childID] = child
		}
		for id := range removeIDs {
			delete(s.data, id)
		}
		if err := s.saveDataLocked(s.data); err != nil {
			s.data = previousData
			return 0, err
		}
	}
	return removed, nil
}
