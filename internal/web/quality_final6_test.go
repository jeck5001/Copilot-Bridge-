package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

func TestConcurrentFailuresGrantExactlyOneImmediateReplay(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b", "account-c")
	if _, err := s.resolveAccount(""); err != nil {
		t.Fatal(err)
	}
	failure := &chathub.RateLimitError{StatusCode: http.StatusTooManyRequests, Reason: "rate limited"}
	const workers = 100
	start := make(chan struct{})
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.markAccountResult("account-a", failure) {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("immediate replay grants=%d; want exactly one", got)
	}
	if active := s.currentActiveAccountID(); active != "account-b" {
		t.Fatalf("active account=%s; want account-b", active)
	}
}

func TestStaleDrainingBindingCannotOverwriteNewGeneration(t *testing.T) {
	store := &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	if _, err := store.upsert(conversation{ID: "thread", AccountID: "account-b", ConversationID: "new", SessionID: "new-session", RouteGeneration: 2}); err != nil {
		t.Fatal(err)
	}
	_, applied, err := store.upsertBinding(conversation{ID: "thread", AccountID: "account-a", ConversationID: "old", SessionID: "old-session", RouteGeneration: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("stale draining generation overwrote the new binding")
	}
	got, _ := store.get("thread")
	if got.AccountID != "account-b" || got.ConversationID != "new" || got.RouteGeneration != 2 {
		t.Fatalf("binding regressed: %+v", got)
	}
}

func TestRouteGenerationSurvivesRestartAndAllowsCurrentBindingUpdate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(root, "sessions.json"))
	routePath := filepath.Join(root, "account-route.json")

	firstSessions, err := openSessionStore()
	if err != nil {
		t.Fatal(err)
	}
	first := newStickyAccountTestServer(t, "account-a", "account-b")
	first.sessions = firstSessions
	first.accountRoutePath = routePath
	if err := first.initializeAccountRouter(); err != nil {
		t.Fatal(err)
	}
	if !first.markAccountResult("account-a", errors.New("HTTP 503 service unavailable")) {
		t.Fatal("failed to advance to account-b")
	}
	if err := first.persistSession("thread", "account-b", "old", chathub.Result{ConversationID: "conversation-old", SessionID: "session-old"}); err != nil {
		t.Fatal(err)
	}
	wantGeneration := first.activeAccountGeneration
	if wantGeneration == 0 {
		t.Fatal("route generation was not initialized")
	}

	restartedSessions, err := openSessionStore()
	if err != nil {
		t.Fatal(err)
	}
	restarted := newStickyAccountTestServer(t, "account-a", "account-b")
	restarted.sessions = restartedSessions
	restarted.accountRoutePath = routePath
	if err := restarted.initializeAccountRouter(); err != nil {
		t.Fatal(err)
	}
	if restarted.activeAccountGeneration != wantGeneration {
		t.Fatalf("restored generation=%d; want %d", restarted.activeAccountGeneration, wantGeneration)
	}
	if err := restarted.persistSession("thread", "account-b", "new", chathub.Result{ConversationID: "conversation-new", SessionID: "session-new"}); err != nil {
		t.Fatal(err)
	}
	got, ok := restarted.sessions.get("thread")
	if !ok || got.AccountID != "account-b" || got.ConversationID != "conversation-new" || got.SessionID != "session-new" {
		t.Fatalf("current binding was rejected after restart: %+v ok=%v", got, ok)
	}
}

func TestAuthClassificationIsTypedAndLegacyNumbersAreBounded(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		if !IsAuthFailure(&chathub.HTTPStatusError{StatusCode: status}) {
			t.Fatalf("typed HTTP %d was not classified as auth", status)
		}
	}
	for _, err := range []error{errors.New("diagnostic 14012"), errors.New("job 4037"), errors.New("trace 40199")} {
		if IsAuthFailure(err) {
			t.Fatalf("unrelated diagnostic number became an auth failure: %v", err)
		}
	}
	if !IsAuthFailure(errors.New("upstream HTTP 401 unauthorized")) {
		t.Fatal("bounded legacy HTTP 401 was not recognized")
	}
}

func TestAuthFailureCannotBeDowngradedByLateSignals(t *testing.T) {
	h := newAccountHealth()
	h.MarkAuthFail("a")
	h.MarkRateLimited("a", time.Now().Add(time.Minute))
	h.MarkDisengaged("a")
	if h.Available("a") {
		t.Fatal("late weaker failure cleared the hard auth pin")
	}
	h.ClearAuthFailure("a")
	if h.Available("a") {
		t.Fatal("clearing auth must retain the independent policy cooldown")
	}
}

func TestHealthDeadlinesMergeMonotonicallyAcrossOutOfOrderSignals(t *testing.T) {
	h := newAccountHealth()
	now := time.Now()
	quotaUntil := now.Add(6 * time.Hour)
	h.MarkQuotaExhausted("a", quotaUntil)
	h.MarkDisengaged("a")
	h.mu.Lock()
	policyUntil := h.transient["a"]
	h.mu.Unlock()

	h.MarkRateLimited("a", now.Add(2*time.Minute))
	h.MarkTransient("a")
	h.MarkAuthFail("a")
	h.mu.Lock()
	gotQuota, gotPolicy := h.cooldown["a"], h.transient["a"]
	authFailed := h.authFail["a"]
	h.mu.Unlock()
	if gotQuota.Before(quotaUntil) {
		t.Fatalf("late 429 shortened quota cooldown to %s; want at least %s", gotQuota, quotaUntil)
	}
	if gotPolicy.Before(policyUntil) {
		t.Fatalf("late transient failure shortened policy rest to %s; want at least %s", gotPolicy, policyUntil)
	}
	if !authFailed {
		t.Fatal("auth failure was not retained")
	}
	h.ClearAuthFailure("a")
	if h.Available("a") {
		t.Fatal("credential refresh bypassed quota/policy rest")
	}
}

func TestOrdinarySuccessDoesNotClearHardOrUnexpiredFailure(t *testing.T) {
	h := newAccountHealth()
	h.MarkAuthFail("a")
	h.MarkQuotaExhausted("a", time.Now().Add(time.Hour))
	h.MarkDisengaged("a")
	h.MarkSuccess("a")
	h.mu.Lock()
	authFailed := h.authFail["a"]
	_, hasCooldown := h.cooldown["a"]
	_, hasTransient := h.transient["a"]
	h.mu.Unlock()
	if !authFailed || !hasCooldown || !hasTransient || h.Available("a") {
		t.Fatalf("late success cleared a newer failure: auth=%t cooldown=%t transient=%t", authFailed, hasCooldown, hasTransient)
	}
}

func TestQuotaBoundaryWithImageIsReusableContent(t *testing.T) {
	res := chathub.Result{Images: []string{"https://example.test/image.png"}, Throttling: map[string]any{"CostQuota": float64(0)}}
	if quotaFailureWithoutContent(res) {
		t.Fatal("valid image at the quota boundary was discarded")
	}
}

func TestQuotaBoundaryWithNativeToolCallIsReusableContent(t *testing.T) {
	tool := chathub.Tool{Type: "function", Function: json.RawMessage(`{"name":"inspect"}`)}
	res := chathub.Result{
		Events:     []json.RawMessage{json.RawMessage(`{"functionName":"inspect","functionArguments":{"path":"."}}`)},
		Throttling: map[string]any{"CostQuota": float64(0)},
	}
	if quotaFailureWithoutContent(res, []chathub.Tool{tool}) {
		t.Fatal("valid native tool call at the quota boundary was discarded")
	}
	if !quotaFailureWithoutContent(res) {
		t.Fatal("unoffered native event was incorrectly accepted as reusable client content")
	}
}

func TestVisibleOutputLimitsHonorAllOpenAIAliases(t *testing.T) {
	for name, request := range map[string]oaiReq{
		"responses":       {MaxOutputTokens: intPointer(7)},
		"chat legacy":     {MaxTokens: intPointer(8)},
		"chat completion": {MaxCompletionTokens: intPointer(9)},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := requestedOutputLimit(request, 16384)
			if err != nil || got < 7 || got > 9 {
				t.Fatalf("limit=%d err=%v", got, err)
			}
		})
	}
	text := "alpha beta gamma delta epsilon zeta eta theta iota kappa"
	clipped, truncated := truncateToOutputLimit("gpt-5.6-sol", text, 3)
	if !truncated || countTokens("gpt-5.6-sol", clipped) > 3 {
		t.Fatalf("clipped=%q tokens=%d truncated=%t", clipped, countTokens("gpt-5.6-sol", clipped), truncated)
	}
}

func TestToolArgumentsCannotBypassVisibleOutputLimit(t *testing.T) {
	calls := []detectedToolCall{{ID: "call_large", Name: "run", Arguments: []byte(`{"command":"this argument cannot fit in one token"}`)}}
	if toolCallsFitOutputLimit("gpt-5.6-sol", calls, 1) {
		t.Fatal("oversized tool call fit a one-token output budget")
	}

	nonStream := httptest.NewRecorder()
	if err := writeToolResponseWithLimit(context.Background(), nonStream, "chatcmpl_limit", "gpt-5.6-sol", false, calls, chathub.Result{}, 1); err != nil {
		t.Fatal(err)
	}
	var completion map[string]any
	if err := json.Unmarshal(nonStream.Body.Bytes(), &completion); err != nil {
		t.Fatal(err)
	}
	encoded := mustJSON(completion)
	if strings.Contains(encoded, "tool_calls") || !strings.Contains(encoded, `"finish_reason":"length"`) {
		t.Fatalf("non-stream tool budget leaked an executable call: %s", encoded)
	}

	chatStream := httptest.NewRecorder()
	if err := writeToolResponseWithLimit(context.Background(), chatStream, "chatcmpl_limit_stream", "gpt-5.6-sol", true, calls, chathub.Result{}, 1); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(chatStream.Body.String(), "tool_calls") {
		t.Fatalf("stream tool budget leaked an executable call: %s", chatStream.Body.String())
	}
	responses := httptest.NewRecorder()
	if err := streamResponsesFromReaderID(responses, strings.NewReader(chatStream.Body.String()), "gpt-5.6-sol", "resp_limit", func(oaiMsg) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(responses.Body.String(), "event: response.incomplete") || strings.Contains(responses.Body.String(), "function_call_arguments.done") {
		t.Fatalf("Responses did not preserve atomic tool truncation: %s", responses.Body.String())
	}
}

func intPointer(value int) *int { return &value }

func TestResponsesRejectsMissingToolOutputAndUnsupportedTool(t *testing.T) {
	missing := responsesRequest{PreviousResponseID: "resp_prev", Input: []any{map[string]any{"type": "function_call_output", "call_id": "call_1"}}}
	if _, err := missing.openAI(); err == nil {
		t.Fatal("missing function output was accepted")
	}
	unsupported := responsesRequest{Input: "hello", Tools: []map[string]any{{"type": "web_search"}}}
	if _, err := unsupported.openAI(); err == nil {
		t.Fatal("unsupported built-in tool was silently ignored")
	}
	valid := responsesRequest{PreviousResponseID: "resp_prev", Input: []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": ""}}}
	if _, err := valid.openAI(); err != nil {
		t.Fatalf("explicit empty-string tool output must remain valid: %v", err)
	}
}

func TestToolResultsMustRemainAdjacentToCalls(t *testing.T) {
	call := map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "run", "arguments": `{}`}}
	messages := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{call}},
		{Role: "user", Content: "interrupt"},
		{Role: "tool", ToolCallID: "call_1", Content: "done"},
	}
	if err := validateToolConversation(messages); err == nil {
		t.Fatal("a user message was allowed inside a pending tool-result sequence")
	}
}

func TestResponsesRejectsUnsupportedSemanticDowngrades(t *testing.T) {
	trueValue := true
	two := 2.0
	for name, request := range map[string]responsesRequest{
		"background":        {Background: &trueValue},
		"temperature":       {Temperature: &two},
		"structured output": {Text: map[string]any{"format": map[string]any{"type": "json_object"}}},
		"truncation":        {Truncation: "auto"},
		"reasoning summary": {Reasoning: &reasoningConfig{Effort: "high", Summary: "detailed"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := request.validateSemantics(map[string]json.RawMessage{}); err == nil {
				t.Fatal("unsupported semantic control was silently accepted")
			}
		})
	}
	falseValue := false
	defaults := responsesRequest{Background: &falseValue, Store: &trueValue, Truncation: "disabled"}
	if err := defaults.validateSemantics(map[string]json.RawMessage{"model": nil}); err != nil {
		t.Fatalf("safe default controls were rejected: %v", err)
	}
	if err := (responsesRequest{}).validateSemantics(map[string]json.RawMessage{"max_output_token": nil}); err == nil {
		t.Fatal("unknown misspelled field was silently ignored")
	}
}

func TestResponsesAcceptsHermesCompatibilityControls(t *testing.T) {
	store := false
	request := responsesRequest{
		Store:     &store,
		Include:   []string{"reasoning.encrypted_content"},
		Reasoning: &reasoningConfig{Effort: "high", Summary: "auto"},
	}
	if err := request.validateSemantics(map[string]json.RawMessage{}); err != nil {
		t.Fatalf("Hermes-compatible request was rejected: %v", err)
	}
	if got := responsesResourceFields(request)["store"]; got != false {
		t.Fatalf("store echo=%v, want false", got)
	}
	request.Include = []string{"file_search_call.results"}
	if err := request.validateSemantics(map[string]json.RawMessage{}); err == nil {
		t.Fatal("unsupported include extension was silently accepted")
	}
}

func TestResponsesResourceFieldsEchoAcrossLifecycle(t *testing.T) {
	parallel := false
	body := responsesRequest{
		Instructions: "policy", Metadata: map[string]string{"trace": "abc"},
		Tools:      []map[string]any{{"type": "function", "name": "inspect"}},
		ToolChoice: "required", ParallelToolCalls: &parallel, PreviousResponseID: "resp_parent",
	}
	fields := responsesResourceFields(body)
	upstream := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	recorder := httptest.NewRecorder()
	if err := streamResponsesFromReaderIDFields(recorder, upstream, "gpt-5.6-sol", "resp_meta", fields, func(oaiMsg) error { return nil }); err != nil {
		t.Fatal(err)
	}
	events := responseStreamEvents(t, recorder.Body.String())
	for _, event := range events {
		response, ok := event["response"].(map[string]any)
		if !ok {
			continue
		}
		metadata, _ := response["metadata"].(map[string]any)
		if metadata["trace"] != "abc" || response["tool_choice"] != "required" || response["parallel_tool_calls"] != false {
			t.Fatalf("resource fields drifted in %v: %#v", event["type"], response)
		}
	}
}
