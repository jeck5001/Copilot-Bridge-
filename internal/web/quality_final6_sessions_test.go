package web

import (
	"context"
	"errors"
	"github.com/vipamess/Copilot-Bridge-/internal/auth"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type timedFailResponseWriter struct {
	header    http.Header
	started   time.Time
	failAfter time.Duration
}

func (w *timedFailResponseWriter) Header() http.Header { return w.header }
func (w *timedFailResponseWriter) WriteHeader(int)     {}
func (w *timedFailResponseWriter) Flush()              {}
func (w *timedFailResponseWriter) Write(p []byte) (int, error) {
	if time.Since(w.started) >= w.failAfter {
		return 0, errors.New("client disconnected")
	}
	return len(p), nil
}

func TestResponsesHeartbeatContinuesWhileSessionCommitWaits(t *testing.T) {
	original := responsesProgressHeartbeatInterval
	responsesProgressHeartbeatInterval = 10 * time.Millisecond
	defer func() { responsesProgressHeartbeatInterval = original }()
	upstream := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	recorder := httptest.NewRecorder()
	err := streamResponsesFromReaderID(recorder, upstream, "test-model", "resp_commit_wait", func(oaiMsg) error {
		time.Sleep(45 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(recorder.Body.String(), "event: response.in_progress"); got < 3 {
		t.Fatalf("commit wait emitted only %d progress events: %s", got, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "event: response.completed") {
		t.Fatalf("missing terminal completion: %s", recorder.Body.String())
	}
}

func TestPrepareResponseTargetDoesNotRewriteSessionStoreBeforeFirstEvent(t *testing.T) {
	store := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	server := &Server{sessions: store}
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer no-prewrite")
	sourceRaw := responseIDSessionKey("resp_source")
	source := scopedSessionKey(request, sourceRaw)
	if _, err := store.upsert(conversation{ID: source, AccountID: "account-a", PortableMessages: []oaiMsg{{Role: "user", Content: "root"}}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	targetRaw := responseIDSessionKey("resp_target")
	discard, err := server.prepareResponseTarget(request, sourceRaw, targetRaw, true)
	if err != nil {
		t.Fatal(err)
	}
	defer discard(true)
	after, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("prepareResponseTarget rewrote the persistent session store")
	}
	if _, ok := store.get(scopedSessionKey(request, targetRaw)); ok {
		t.Fatal("target materialized before commit")
	}
	if err := server.commitResponsePortableHistory(request, sourceRaw, targetRaw, []oaiMsg{{Role: "user", Content: "next"}}, oaiMsg{Role: "assistant", Content: "answer"}, false); err != nil {
		t.Fatal(err)
	}
}

func TestCommitWaitJoinsPersistenceAfterClientWriteFailure(t *testing.T) {
	original := responsesProgressHeartbeatInterval
	responsesProgressHeartbeatInterval = 5 * time.Millisecond
	defer func() { responsesProgressHeartbeatInterval = original }()
	upstream := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	w := &timedFailResponseWriter{header: make(http.Header), started: time.Now(), failAfter: 12 * time.Millisecond}
	started := time.Now()
	err := streamResponsesFromReaderID(w, upstream, "test-model", "resp_disconnect_commit", func(oaiMsg) error {
		time.Sleep(45 * time.Millisecond)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "client disconnected") {
		t.Fatalf("expected client disconnect, got %v", err)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond {
		t.Fatalf("returned before persistence joined: %s", elapsed)
	}
}

func TestProducerMaterializedResponseInheritsSourceAliasGroupAndHistory(t *testing.T) {
	store := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	server := &Server{sessions: store}
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer alias-group")
	sourceRaw := responseIDSessionKey("resp_parent")
	targetRaw := responseIDSessionKey("resp_child")
	source := scopedSessionKey(request, sourceRaw)
	target := scopedSessionKey(request, targetRaw)
	if _, err := store.upsert(conversation{
		ID: source, AccountID: "old-account", AliasGroup: "stable-root", ResponseAlias: true,
		PortableMessages: []oaiMsg{{Role: "user", Content: "ROOT_CONTEXT"}},
	}); err != nil {
		t.Fatal(err)
	}
	finish, err := server.prepareResponseTarget(request, sourceRaw, targetRaw, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.upsert(conversation{
		ID: target, AccountID: "new-account", ConversationID: "new-conversation", SessionID: "new-session",
		PortableMessages: []oaiMsg{{Role: "tool", ToolCallID: "call_1", Content: "TOOL_RESULT"}},
	}); err != nil {
		finish(false)
		t.Fatal(err)
	}
	if err := server.commitResponsePortableHistory(request, sourceRaw, targetRaw, nil, oaiMsg{Role: "assistant", Content: "FINAL_ANSWER"}, false); err != nil {
		finish(false)
		t.Fatal(err)
	}
	finish(true)
	child, ok := store.get(target)
	if !ok {
		t.Fatal("committed response alias missing")
	}
	if child.AliasGroup != "stable-root" || !child.ResponseAlias {
		t.Fatalf("alias classification=%q responseAlias=%v", child.AliasGroup, child.ResponseAlias)
	}
	if child.AccountID != "new-account" || child.ConversationID != "new-conversation" || child.SessionID != "new-session" {
		t.Fatalf("producer binding was overwritten: %+v", child)
	}
	history := mustJSON(child.PortableMessages)
	for _, marker := range []string{"ROOT_CONTEXT", "TOOL_RESULT", "FINAL_ANSWER"} {
		if !strings.Contains(history, marker) {
			t.Fatalf("committed history missing %q: %s", marker, history)
		}
	}
}

func TestPreparedPreviousResponseIsPinnedUntilCompletion(t *testing.T) {
	store := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	server := &Server{sessions: store}
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer pinned-source")
	sourceRaw := responseIDSessionKey("resp_pinned")
	targetRaw := responseIDSessionKey("resp_target")
	source := scopedSessionKey(request, sourceRaw)
	if _, err := store.upsert(conversation{ID: source, ResponseAlias: true, AliasGroup: "root", PortableMessages: []oaiMsg{{Role: "user", Content: "PIN_ME"}}}); err != nil {
		t.Fatal(err)
	}
	finish, err := server.prepareResponseTarget(request, sourceRaw, targetRaw, true)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	stale := store.data[source]
	stale.UpdatedAt = time.Now().Add(-sessionTTL - time.Hour)
	store.data[source] = stale
	store.pruneLocked(time.Now())
	_, retained := store.data[source]
	store.mu.Unlock()
	if !retained {
		finish(false)
		t.Fatal("active previous_response_id was pruned")
	}
	finish(false)
}

func TestStableResponseSessionRejectsConcurrentWriterWithoutWaiting(t *testing.T) {
	store := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	server := &Server{sessions: store}
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer stable-concurrency")
	sourceRaw := "responses_stable_key"
	firstTarget := responseIDSessionKey("resp_first")
	secondTarget := responseIDSessionKey("resp_second")
	finishFirst, err := server.prepareResponseTarget(request, sourceRaw, firstTarget, false)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := server.prepareResponseTarget(request, sourceRaw, secondTarget, false); err == nil {
		finishFirst(false)
		t.Fatal("concurrent stable writer was accepted")
	} else if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		finishFirst(false)
		t.Fatalf("concurrent stable writer blocked for %s", elapsed)
	}
	finishFirst(false)
	finishSecond, err := server.prepareResponseTarget(request, sourceRaw, secondTarget, false)
	if err != nil {
		t.Fatalf("stable writer remained locked after completion: %v", err)
	}
	finishSecond(false)
}

func TestInactiveAccountResponseCannotAdvanceStableHead(t *testing.T) {
	dir := t.TempDir()
	tokens, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	active, err := tokens.Upsert(auth.TokenSet{HomeOID: "account-active", AccessToken: "token-a", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	inactive, err := tokens.Upsert(auth.TokenSet{HomeOID: "account-inactive", AccessToken: "token-b", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	store := &sessionStore{path: filepath.Join(dir, "sessions.json"), data: map[string]conversation{}}
	server := &Server{
		tokens: tokens, sessions: store, accountRoutePath: filepath.Join(dir, "route.json"),
		activeAccountLeases: map[uint64]context.CancelFunc{},
	}
	if err := server.initializeAccountRouter(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer inactive-commit")
	sourceRaw := "responses_stable_head"
	targetRaw := responseIDSessionKey("resp_inactive")
	source := scopedSessionKey(request, sourceRaw)
	target := scopedSessionKey(request, targetRaw)
	if _, err := store.upsert(conversation{ID: source, AccountID: active.ID, PortableMessages: []oaiMsg{{Role: "assistant", Content: "ACTIVE_HEAD"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.upsert(conversation{ID: target, AccountID: inactive.ID, PortableMessages: []oaiMsg{{Role: "assistant", Content: "STALE_BRANCH"}}}); err != nil {
		t.Fatal(err)
	}
	if err := server.commitResponsePortableHistory(request, sourceRaw, targetRaw, nil, oaiMsg{Role: "assistant", Content: "LATE_RESULT"}, true); err != nil {
		t.Fatal(err)
	}
	head, ok := store.get(source)
	if !ok {
		t.Fatal("stable head disappeared")
	}
	if head.AccountID != active.ID || !strings.Contains(mustJSON(head.PortableMessages), "ACTIVE_HEAD") || strings.Contains(mustJSON(head.PortableMessages), "LATE_RESULT") {
		t.Fatalf("inactive response overwrote active stable head: %+v", head)
	}
	branch, ok := store.get(target)
	if !ok || !strings.Contains(mustJSON(branch.PortableMessages), "LATE_RESULT") {
		t.Fatalf("immutable inactive response branch was not retained: %+v", branch)
	}
}
