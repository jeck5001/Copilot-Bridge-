package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSessionPersistenceFailureDoesNotCommit(t *testing.T) {
	s := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	if _, err := s.upsert(conversation{ID: "good", ConversationID: "c", SessionID: "s", Title: strings.Repeat("x", 1000)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.get("good"); len(got.Title) > 240 {
		t.Fatalf("title not bounded: %d", len(got.Title))
	}
	s.path = string([]byte{0})
	// Force pruneLocked to remove an unrelated record before the write fails.
	// Transaction rollback must restore the whole map, not just "bad".
	s.data["expired-but-committed"] = conversation{ID: "expired-but-committed", UpdatedAt: time.Now().Add(-sessionTTL - time.Hour)}
	if _, err := s.upsert(conversation{ID: "bad", ConversationID: "c", SessionID: "s"}); err == nil {
		t.Fatal("expected persistence error")
	}
	if _, ok := s.get("bad"); ok {
		t.Fatal("failed write committed in-memory session")
	}
	if _, ok := s.get("expired-but-committed"); !ok {
		t.Fatal("failed write did not roll back unrelated entries removed by pruning")
	}
}

func TestResponseBranchesRemainImmutableAndIndependent(t *testing.T) {
	store := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	server := &Server{sessions: store}
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer branch-key")
	sourceRaw := responseIDSessionKey("resp_parent")
	source := scopedSessionKey(request, sourceRaw)
	if _, err := store.upsert(conversation{ID: source, AccountID: "account-a", PortableMessages: []oaiMsg{{Role: "user", Content: "ROOT_MARKER"}}, ResponseAlias: true, AliasGroup: "root"}); err != nil {
		t.Fatal(err)
	}

	for i, marker := range []string{"BRANCH_ONE", "BRANCH_TWO"} {
		targetRaw := responseIDSessionKey(fmt.Sprintf("resp_child_%d", i))
		discard, err := server.prepareResponseTarget(request, sourceRaw, targetRaw, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := server.commitResponsePortableHistory(request, sourceRaw, targetRaw, []oaiMsg{{Role: "user", Content: marker}}, oaiMsg{Role: "assistant", Content: "ANSWER_" + marker}, false); err != nil {
			discard(false)
			t.Fatal(err)
		}
		discard(true)
	}

	parent, _ := store.get(source)
	parentJSON := mustJSON(parent.PortableMessages)
	if strings.Contains(parentJSON, "BRANCH_ONE") || strings.Contains(parentJSON, "BRANCH_TWO") {
		t.Fatalf("immutable parent was mutated: %s", parentJSON)
	}
	for i, marker := range []string{"BRANCH_ONE", "BRANCH_TWO"} {
		target := scopedSessionKey(request, responseIDSessionKey(fmt.Sprintf("resp_child_%d", i)))
		child, ok := store.get(target)
		if !ok {
			t.Fatalf("missing branch %d", i)
		}
		serialized := mustJSON(child.PortableMessages)
		other := []string{"BRANCH_TWO", "BRANCH_ONE"}[i]
		if !strings.Contains(serialized, "ROOT_MARKER") || !strings.Contains(serialized, marker) || strings.Contains(serialized, other) {
			t.Fatalf("branch %d is contaminated: %s", i, serialized)
		}
	}
}

func TestResponseCommitFailureRollsBackTargetAndStableHead(t *testing.T) {
	store := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	now := time.Now().UTC()
	store.data["stable"] = conversation{ID: "stable", PortableMessages: []oaiMsg{{Role: "user", Content: "OLD_HEAD"}}, UpdatedAt: now}
	store.data["target"] = conversation{ID: "target", PortableMessages: []oaiMsg{{Role: "user", Content: "OLD_TARGET"}}, UpdatedAt: now, ResponseAlias: true, AliasGroup: "stable"}
	store.path = string([]byte{0})
	if _, err := store.commitResponse("stable", "target", []oaiMsg{{Role: "user", Content: "NEW_INPUT"}}, oaiMsg{Role: "assistant", Content: "NEW_OUTPUT"}, true); err == nil {
		t.Fatal("expected persistence failure")
	}
	for id, marker := range map[string]string{"stable": "OLD_HEAD", "target": "OLD_TARGET"} {
		value, ok := store.get(id)
		if !ok || !strings.Contains(mustJSON(value.PortableMessages), marker) || strings.Contains(mustJSON(value.PortableMessages), "NEW_") {
			t.Fatalf("%s was partially committed: %+v", id, value)
		}
	}
}

func TestResponseAliasWindowRetainsStableAndRecentPreviousIDs(t *testing.T) {
	store := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	server := &Server{sessions: store}
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer alias-window-key")
	sourceRaw := "responses_stable_thread"
	source := scopedSessionKey(request, sourceRaw)
	portable := []oaiMsg{
		{Role: "user", Content: "keep this goal"},
		{Role: "assistant", Content: "keep this answer"},
	}
	if _, err := store.upsert(conversation{
		ID: source, AccountID: "account-a", ConversationID: "conversation-a",
		SessionID: "session-a", PortableMessages: portable,
	}); err != nil {
		t.Fatal(err)
	}

	const rounds = 400
	responseIDs := make([]string, 0, rounds)
	for i := 0; i < rounds; i++ {
		responseID := fmt.Sprintf("resp_%03d", i)
		if err := server.aliasResponseSession(request, sourceRaw, responseID); err != nil {
			t.Fatal(err)
		}
		responseIDs = append(responseIDs, responseID)
		sourceRaw = responseIDSessionKey(responseID)
	}
	if _, ok := store.get(source); !ok {
		t.Fatal("stable Responses key was evicted by ordinary response aliases")
	}
	if got := len(store.data); got != 1+maxResponseAliasesPerSession {
		t.Fatalf("stored sessions=%d want=%d", got, 1+maxResponseAliasesPerSession)
	}
	for _, responseID := range responseIDs[rounds-maxResponseAliasesPerSession:] {
		alias := scopedSessionKey(request, responseIDSessionKey(responseID))
		if value, ok := store.get(alias); !ok || value.AliasGroup != source || !value.ResponseAlias {
			t.Fatalf("recent previous_response_id %s unavailable or misclassified: %+v ok=%v", responseID, value, ok)
		}
	}
	oldest := scopedSessionKey(request, responseIDSessionKey(responseIDs[0]))
	if _, ok := store.get(oldest); ok {
		t.Fatal("alias outside the bounded previous_response_id window was retained")
	}
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxSessionStoreBytes {
		t.Fatalf("sessions.json=%d bytes exceeds cap=%d", len(raw), maxSessionStoreBytes)
	}
}

func TestPortableHistoryBoundsPreserveToolSequence(t *testing.T) {
	messages := make([]oaiMsg, 0, 360)
	messages = append(messages, oaiMsg{Role: "user", Content: "ONE_LONG_AGENT_TASK"})
	for i := 0; i < 120; i++ {
		callID := fmt.Sprintf("call_%03d", i)
		messages = append(messages,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{
				"id": callID, "type": "function",
				"function": map[string]any{"name": "inspect", "arguments": fmt.Sprintf(`{"turn":%d,"padding":"%s"}`, i, strings.Repeat("u", 800))},
			}}},
			oaiMsg{Role: "tool", ToolCallID: callID, Content: fmt.Sprintf("RESULT_%03d_%s", i, strings.Repeat("r", 1200))},
		)
	}
	bounded := boundedPortableMessages(messages)
	if len(bounded) > maxPortableMessages {
		t.Fatalf("portable messages=%d cap=%d", len(bounded), maxPortableMessages)
	}
	if size := portableMessagesBytes(bounded); size > maxPortableHistoryBytes {
		t.Fatalf("portable bytes=%d cap=%d", size, maxPortableHistoryBytes)
	}
	if len(bounded) == 0 || bounded[0].Role != "user" {
		t.Fatalf("bounded history starts mid tool sequence: %+v", bounded)
	}
	if err := validateToolConversation(bounded); err != nil {
		t.Fatalf("bounded history broke user->assistant call->tool result order: %v", err)
	}
	if len(bounded) != len(messages) {
		t.Fatalf("128-round-capable history dropped a valid 120-round task: got=%d want=%d", len(bounded), len(messages))
	}
	if text := mustJSON(bounded); !strings.Contains(text, "RESULT_000_") || !strings.Contains(text, "RESULT_119_") {
		t.Fatal("portable history did not retain the earliest and latest tool evidence")
	}
	prompt, _ := flattenPromptMessages(bounded, nil)
	for _, marker := range []string{"ONE_LONG_AGENT_TASK", "call_119", "RESULT_119"} {
		if !strings.Contains(prompt, marker) {
			t.Fatalf("recent tool sequence missing %s", marker)
		}
	}
}

func TestPortableHistoryKeepsMaximumParallelToolCalls(t *testing.T) {
	calls := make([]map[string]any, 0, 64)
	for i := 0; i < 64; i++ {
		calls = append(calls, map[string]any{
			"id":   fmt.Sprintf("call_%02d", i),
			"type": "function",
			"function": map[string]any{
				"name":      "inspect",
				"arguments": fmt.Sprintf(`{"index":%d}`, i),
			},
		})
	}
	messages := []oaiMsg{
		{Role: "user", Content: "inspect everything"},
		{Role: "assistant", ToolCalls: calls},
	}
	for i := 0; i < 64; i++ {
		messages = append(messages, oaiMsg{Role: "tool", ToolCallID: fmt.Sprintf("call_%02d", i), Content: "ok"})
	}
	bounded := boundedPortableMessages(messages)
	if len(bounded) != 66 {
		t.Fatalf("portable history messages=%d; want 66", len(bounded))
	}
	if got := len(bounded[1].ToolCalls); got != 64 {
		t.Fatalf("portable history retained %d parallel calls; want 64", got)
	}
	if err := validateToolConversation(bounded); err != nil {
		t.Fatalf("restored 64-call turn is not valid: %v", err)
	}
}

func TestSessionStoreHardSerializedByteCapPrefersAliasEviction(t *testing.T) {
	now := time.Now().UTC()
	data := map[string]conversation{
		"stable": {ID: "stable", AliasGroup: "stable", UpdatedAt: now, PortableMessages: []oaiMsg{{Role: "user", Content: "stable"}}},
	}
	large := boundedPortableMessages([]oaiMsg{{Role: "user", Content: strings.Repeat("x", maxPortableContentBytes)}})
	for i := 0; i < 1100; i++ {
		id := fmt.Sprintf("alias-%04d", i)
		data[id] = conversation{ID: id, AliasGroup: id, ResponseAlias: true, UpdatedAt: now.Add(time.Duration(i) * time.Millisecond), PortableMessages: large}
	}
	store := &sessionStore{data: data}
	store.pruneLocked(now.Add(time.Hour))
	encoded, err := json.MarshalIndent(store.data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxSessionStoreBytes {
		t.Fatalf("serialized store=%d cap=%d", len(encoded), maxSessionStoreBytes)
	}
	if _, ok := store.data["stable"]; !ok {
		t.Fatal("stable session was evicted while response aliases remained")
	}
}

func TestOpenSessionStoreMigratesLegacyDuplicateAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	now := time.Now().UTC()
	legacy := map[string]conversation{}
	for i := 0; i < maxResponseAliasesPerSession+48; i++ {
		id := fmt.Sprintf("legacy-%03d", i)
		legacy[id] = conversation{
			ID: id, AccountID: "account-a", ConversationID: "conversation-a", SessionID: "session-a",
			CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now.Add(time.Duration(i) * time.Second),
		}
	}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_SESSION_CACHE", path)
	store, err := openSessionStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(store.data); got != 1+maxResponseAliasesPerSession {
		t.Fatalf("legacy duplicate group retained %d entries", got)
	}
	stable := 0
	groups := map[string]bool{}
	for _, value := range store.data {
		groups[value.AliasGroup] = true
		if !value.ResponseAlias {
			stable++
		}
	}
	if stable != 1 || len(groups) != 1 {
		t.Fatalf("legacy migration stable=%d groups=%d", stable, len(groups))
	}
}

func TestSessionStorePrunesExpired(t *testing.T) {
	s := &sessionStore{data: map[string]conversation{
		"old": {ID: "old", UpdatedAt: time.Now().Add(-sessionTTL - time.Hour)},
		"new": {ID: "new", UpdatedAt: time.Now()},
	}}
	s.pruneLocked(time.Now())
	if _, ok := s.data["old"]; ok {
		t.Fatal("expired session retained")
	}
	if _, ok := s.data["new"]; !ok {
		t.Fatal("current session removed")
	}
}

func TestLegacyPortableCopiesMigrateToParentDelta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	now := time.Now().UTC()
	history := []oaiMsg{{Role: "user", Content: "旧上下文"}, {Role: "assistant", Content: "旧回答"}}
	legacy := map[string]conversation{}
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("legacy-copy-%d", i)
		legacy[id] = conversation{
			ID: id, AccountID: "account", ConversationID: "conversation", SessionID: "session",
			PortableMessages: history, CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now.Add(time.Duration(i) * time.Second),
		}
	}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_SESSION_CACHE", path)
	store, err := openSessionStore()
	if err != nil {
		t.Fatal(err)
	}
	materializedRoots := 0
	for id, value := range store.data {
		if len(value.PortableMessages) > 0 {
			materializedRoots++
		}
		got, ok := store.get(id)
		if !ok || !strings.Contains(mustJSON(got.PortableMessages), "旧上下文") || !strings.Contains(mustJSON(got.PortableMessages), "旧回答") {
			t.Fatalf("legacy snapshot %s was not preserved: %+v", id, got)
		}
	}
	if materializedRoots != 1 {
		t.Fatalf("legacy history copies retained=%d want=1", materializedRoots)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if copies := strings.Count(string(persisted), `"portableMessages"`); copies != 1 {
		t.Fatalf("persisted portable history copies=%d want=1", copies)
	}
}

func TestResponseParentDeltaBranchesRemainImmutable(t *testing.T) {
	store := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	if _, err := store.upsert(conversation{ID: "stable", PortableMessages: []oaiMsg{{Role: "user", Content: "root"}}}); err != nil {
		t.Fatal(err)
	}
	first, err := store.commitResponse("stable", "resp-1", nil, oaiMsg{Role: "assistant", Content: "first"}, true)
	if err != nil {
		t.Fatal(err)
	}
	branchA, err := store.commitResponse("resp-1", "resp-a", []oaiMsg{{Role: "user", Content: "branch-a"}}, oaiMsg{Role: "assistant", Content: "answer-a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	branchB, err := store.commitResponse("resp-1", "resp-b", []oaiMsg{{Role: "user", Content: "branch-b"}}, oaiMsg{Role: "assistant", Content: "answer-b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.data["resp-a"].PortableMessages) != 0 || store.data["resp-a"].HistoryParent != "resp-1" {
		t.Fatalf("branch A was not stored as delta: %+v", store.data["resp-a"])
	}
	if len(store.data["stable"].PortableMessages) != 0 || store.data["stable"].HistoryParent != "resp-1" {
		t.Fatalf("stable head duplicated history: %+v", store.data["stable"])
	}
	for label, value := range map[string]conversation{"first": first, "a": branchA, "b": branchB} {
		encoded := mustJSON(value.PortableMessages)
		if !strings.Contains(encoded, "root") || !strings.Contains(encoded, "first") {
			t.Fatalf("%s lost parent history: %s", label, encoded)
		}
	}
	if strings.Contains(mustJSON(branchA.PortableMessages), "branch-b") || strings.Contains(mustJSON(branchB.PortableMessages), "branch-a") {
		t.Fatalf("branches contaminated each other: a=%s b=%s", mustJSON(branchA.PortableMessages), mustJSON(branchB.PortableMessages))
	}
	firstAgain, _ := store.get("resp-1")
	if strings.Contains(mustJSON(firstAgain.PortableMessages), "branch-") {
		t.Fatalf("immutable parent changed after branching: %s", mustJSON(firstAgain.PortableMessages))
	}
}

func TestCommittedResponseWindowUsesSingleMaterializedHistoryRoot(t *testing.T) {
	store := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	if _, err := store.upsert(conversation{ID: "stable", PortableMessages: []oaiMsg{{Role: "user", Content: "long-chain-root"}}}); err != nil {
		t.Fatal(err)
	}
	const rounds = maxResponseAliasesPerSession + 12
	for i := 0; i < rounds; i++ {
		target := fmt.Sprintf("response-%03d", i)
		if _, err := store.commitResponse("stable", target, nil, oaiMsg{Role: "assistant", Content: fmt.Sprintf("answer-%03d", i)}, true); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(store.data); got != 1+maxResponseAliasesPerSession {
		t.Fatalf("stored sessions=%d want=%d", got, 1+maxResponseAliasesPerSession)
	}
	materializedRoots := 0
	for _, value := range store.data {
		if len(value.PortableMessages) > 0 {
			materializedRoots++
		}
	}
	if materializedRoots != 1 {
		t.Fatalf("materialized history roots=%d want=1", materializedRoots)
	}
	latest, ok := store.get(fmt.Sprintf("response-%03d", rounds-1))
	if !ok {
		t.Fatal("latest response missing")
	}
	encoded := mustJSON(latest.PortableMessages)
	if !strings.Contains(encoded, "long-chain-root") || !strings.Contains(encoded, fmt.Sprintf("answer-%03d", rounds-1)) {
		t.Fatalf("latest compact chain did not materialize: %s", encoded)
	}
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxSessionStoreBytes || strings.Count(string(raw), `"portableMessages"`) != 1 {
		t.Fatalf("unexpected compact store bytes=%d portableCopies=%d", len(raw), strings.Count(string(raw), `"portableMessages"`))
	}
}

func TestPinnedAliasProtectsItsParentChainDuringConcurrentPrune(t *testing.T) {
	store := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	if _, err := store.upsert(conversation{ID: "root", AliasGroup: "chain", ResponseAlias: true, PortableMessages: []oaiMsg{{Role: "user", Content: "root"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.commitResponse("root", "child", nil, oaiMsg{Role: "assistant", Content: "child"}, false); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	pinned := make(chan struct{})
	release := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, ok := store.pin("child"); !ok {
			t.Errorf("could not pin child")
			close(pinned)
			return
		}
		close(pinned)
		<-release
		store.unpin("child")
	}()
	<-pinned
	store.mu.Lock()
	root := store.data["root"]
	root.UpdatedAt = time.Now().Add(-sessionTTL - time.Hour)
	store.data["root"] = root
	store.pruneLocked(time.Now())
	_, rootRetained := store.data["root"]
	_, childRetained := store.data["child"]
	store.mu.Unlock()
	close(release)
	wg.Wait()
	if !rootRetained || !childRetained {
		t.Fatalf("pinned lineage was pruned: root=%v child=%v", rootRetained, childRetained)
	}
}

func TestPortableUTF8BoundaryRemainsValid(t *testing.T) {
	input := strings.Repeat("中", (maxPortableContentBytes/3)+100)
	bounded := boundedPortableMessages([]oaiMsg{{Role: "user", Content: input}})
	if len(bounded) != 1 {
		t.Fatalf("bounded messages=%d", len(bounded))
	}
	text, _ := bounded[0].Content.(string)
	if !utf8.ValidString(text) || strings.ContainsRune(text, '\uFFFD') {
		t.Fatalf("portable truncation produced invalid UTF-8")
	}
	if len(text) > maxPortableContentBytes {
		t.Fatalf("portable content bytes=%d cap=%d", len(text), maxPortableContentBytes)
	}
}

func TestPortableHistoryRetainsConfiguredSequentialToolRounds(t *testing.T) {
	messages := []oaiMsg{{Role: "user", Content: "512-round goal"}}
	for i := 0; i < maxRecoverableToolRounds; i++ {
		id := fmt.Sprintf("call_%d", i)
		messages = append(messages,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "step", "arguments": `{}`}}}},
			oaiMsg{Role: "tool", ToolCallID: id, Content: "ok"},
		)
	}
	bounded := boundedPortableMessages(messages)
	if len(bounded) != len(messages) {
		t.Fatalf("recoverable rounds were truncated: messages=%d want=%d bytes=%d", len(bounded), len(messages), portableMessagesBytes(bounded))
	}
	if err := validateToolConversation(bounded); err != nil {
		t.Fatalf("restored 512-round transcript invalid: %v", err)
	}
}

func TestCausalTrimmingNeverSkipsRecentOversizedUnitForOlderUnit(t *testing.T) {
	messages := []oaiMsg{
		{Role: "user", Content: "goal"},
		{Role: "assistant", Content: "older-small-unit"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "bulk", "type": "function", "function": map[string]any{"name": "bulk", "arguments": `{}`}}}},
	}
	for i := 0; i < maxPortableToolCalls*2; i++ {
		messages = append(messages, oaiMsg{Role: "tool", ToolCallID: "bulk", Content: strings.Repeat("x", maxPortableContentBytes)})
	}
	bounded := boundedPortableMessages(messages)
	encoded := mustJSON(bounded)
	if strings.Contains(encoded, "older-small-unit") {
		t.Fatalf("older unit was retained across a missing newer causal unit")
	}
	if len(bounded) != 1 || bounded[0].Role != "user" {
		t.Fatalf("oversized newest unit should leave only the goal anchor: len=%d", len(bounded))
	}
}
