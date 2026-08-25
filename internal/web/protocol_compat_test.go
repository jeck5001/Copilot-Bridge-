package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesToOpenAI(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "what time", Tools: []map[string]any{{"type": "function", "name": "clock", "parameters": map[string]any{"type": "object"}}}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 1 || len(o.Tools) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestResponsesPreservesInstructionsEveryTurn(t *testing.T) {
	parallel := false
	maxOutput := 8192
	r := responsesRequest{
		Model:              "gpt-5.6-sol",
		Instructions:       "WORK UNTIL THE TASK IS VERIFIED. Use tools when required.",
		Input:              "continue",
		PreviousResponseID: "resp_123",
		ParallelToolCalls:  &parallel,
		MaxOutputTokens:    &maxOutput,
	}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.Instructions != r.Instructions || len(o.Messages) != 1 {
		t.Fatalf("responses instructions were not preserved separately from portable history: instructions=%q messages=%#v", o.Instructions, o.Messages)
	}
	execution := withRequestInstructions(o.Messages, o.Instructions)
	if len(execution) != 2 || execution[0].Role != "developer" || execution[0].Content != r.Instructions {
		t.Fatalf("responses instructions were not applied as highest-priority execution context: %#v", execution)
	}
	if o.ParallelToolCalls == nil || *o.ParallelToolCalls || o.MaxOutputTokens == nil || *o.MaxOutputTokens != maxOutput {
		t.Fatalf("responses controls were dropped: parallel=%v max_output=%v", o.ParallelToolCalls, o.MaxOutputTokens)
	}
}

func TestResponsesRejectsInvalidSemanticControls(t *testing.T) {
	zero := 0
	for name, request := range map[string]responsesRequest{
		"instructions": {Instructions: map[string]any{"unexpected": true}, Input: "hello"},
		"max_output":   {MaxOutputTokens: &zero, Input: "hello"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := request.openAI(); err == nil {
				t.Fatal("expected invalid semantic control to be rejected")
			}
		})
	}
}

func TestOpenCodeNullAssistantContentDoesNotBecomeNilText(t *testing.T) {
	message := oaiMsg{Role: "assistant", Content: nil, ToolCalls: []map[string]any{{
		"id": "call_1", "type": "function", "function": map[string]any{"name": "exec", "arguments": `{\"cmd\":\"go test ./...\"}`},
	}}}
	prompt, _ := flattenPromptMessages([]oaiMsg{message}, nil)
	if strings.Contains(prompt, "<nil>") || !strings.Contains(prompt, "call_1") || !strings.Contains(prompt, "go test ./...") {
		t.Fatalf("OpenCode tool-only assistant message was corrupted: %q", prompt)
	}
}

func TestAnthropicToOpenAI(t *testing.T) {
	r := anthropicRequest{Model: "m", System: any("be concise"), Messages: []anthropicMessage{{Role: "user", Content: any("weather")}}, Tools: []anthropicTool{{Name: "weather", InputSchema: map[string]any{"type": "object"}}}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 2 || len(o.Tools) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestAnthropicToolResult(t *testing.T) {
	r := anthropicRequest{Messages: []anthropicMessage{{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "x", "name": "f", "input": map[string]any{}}}}, {Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": "ok"}}}}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 2 || o.Messages[1].ToolCallID != "x" {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestResponsesDerivesOpaqueStableSessionKey(t *testing.T) {
	r := responsesRequest{PromptCacheKey: "thread-secret", ClientMetadata: map[string]any{"thread_id": "fallback"}}
	key := r.stableSessionKey()
	if key == "" || key == "thread-secret" || key != r.stableSessionKey() {
		t.Fatalf("unstable or non-opaque key: %q", key)
	}
	r.NewConversation = true
	if key := r.stableSessionKey(); key != "" {
		t.Fatalf("new conversation reused session: %q", key)
	}
}

func TestResponsesAcceptsConversationObject(t *testing.T) {
	r := responsesRequest{Conversation: map[string]any{"id": "conv-1"}}
	if key := r.stableSessionKey(); key == "" || key == "conv-1" {
		t.Fatalf("conversation object did not produce opaque key: %q", key)
	}
}

func TestResponsesGroupsAdjacentParallelFunctionCalls(t *testing.T) {
	r := responsesRequest{Input: []any{
		map[string]any{"type": "message", "role": "user", "content": "inspect both"},
		map[string]any{"type": "function_call", "call_id": "c1", "name": "inspect", "arguments": `{"path":"a"}`},
		map[string]any{"type": "function_call", "call_id": "c2", "name": "inspect", "arguments": `{"path":"b"}`},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "A"},
		map[string]any{"type": "function_call_output", "call_id": "c2", "output": "B"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 4 || len(o.Messages[1].ToolCalls) != 2 {
		t.Fatalf("parallel calls were not grouped: %#v", o.Messages)
	}
	if err := validateToolConversation(o.Messages); err != nil {
		t.Fatalf("converted parallel turn is invalid: %v", err)
	}
}

func TestResponsesRejectsPreviousResponseAndConversationTogether(t *testing.T) {
	for _, conversation := range []any{"conv_123", map[string]any{"id": "conv_123"}} {
		r := responsesRequest{Input: "continue", PreviousResponseID: "resp_123", Conversation: conversation}
		_, err := r.openAI()
		if err == nil || !strings.Contains(err.Error(), "cannot both be provided") {
			t.Fatalf("expected conflict for %#v, got %v", conversation, err)
		}
	}
}

func TestResponsesPreviousResponseTakesSessionPrecedence(t *testing.T) {
	r := responsesRequest{Input: "continue", PreviousResponseID: "resp_123", PromptCacheKey: "stable-thread"}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.SessionKey != responseIDSessionKey("resp_123") {
		t.Fatalf("previous response did not select response alias: %q", o.SessionKey)
	}
	r.NewConversation = true
	if _, err := r.openAI(); err == nil {
		t.Fatal("previous_response_id with new_conversation must be rejected")
	}
}

func TestResponsesToolOutputContinuationAllowsPriorCallAlias(t *testing.T) {
	r := responsesRequest{Input: []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"}}, PreviousResponseID: "resp_123"}
	o, err := r.openAI()
	if err != nil || !o.AllowResponsesToolContinuation {
		t.Fatalf("continuation marker missing: %+v err=%v", o, err)
	}
	if !allowResponsesToolContinuation(o) {
		t.Fatal("Responses tool continuation was not recognized")
	}
}

func TestResponseAliasIsCredentialScoped(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer key-a")
	sourceRaw := "responses_source"
	source := scopedSessionKey(request, sourceRaw)
	store := &sessionStore{path: t.TempDir() + "/sessions.json", data: map[string]conversation{}}
	if _, err := store.upsert(conversation{ID: source, AccountID: "a", ConversationID: "c", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	server := &Server{sessions: store}
	if err := server.aliasResponseSession(request, sourceRaw, "resp_123"); err != nil {
		t.Fatal(err)
	}
	alias := scopedSessionKey(request, responseIDSessionKey("resp_123"))
	got, ok := store.get(alias)
	if !ok || got.ConversationID != "c" || got.SessionID != "s" {
		t.Fatalf("response alias missing: %#v ok=%v", got, ok)
	}
	other := httptest.NewRequest("POST", "/v1/responses", nil)
	other.Header.Set("Authorization", "Bearer key-b")
	if _, ok := store.get(scopedSessionKey(other, responseIDSessionKey("resp_123"))); ok {
		t.Fatal("response alias crossed API credentials")
	}
}

func TestResponseAliasAcceptsFirstResponseIdentityRoot(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer key-a")
	responseID := "resp_first"
	sourceRaw := responseIDSessionKey(responseID)
	source := scopedSessionKey(request, sourceRaw)
	store := &sessionStore{path: t.TempDir() + "/sessions.json", data: map[string]conversation{}}
	if _, err := store.upsert(conversation{ID: source, AccountID: "a", ConversationID: "c", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	server := &Server{sessions: store}
	if err := server.aliasResponseSession(request, sourceRaw, responseID); err != nil {
		t.Fatalf("identity alias rejected: %v", err)
	}
	if got, ok := store.get(source); !ok || got.ConversationID != "c" || got.SessionID != "s" {
		t.Fatalf("identity-rooted response state changed or disappeared: %#v ok=%v", got, ok)
	}
}

func TestResponsesStableSessionContinuesAndIsCredentialScoped(t *testing.T) {
	o1, err1 := (responsesRequest{Input: "one", ClientMetadata: map[string]any{"thread_id": "stable-thread"}}).openAI()
	o2, err2 := (responsesRequest{Input: "two", ClientMetadata: map[string]any{"thread_id": "stable-thread"}}).openAI()
	if err1 != nil || err2 != nil || o1.SessionKey == "" || o1.SessionKey != o2.SessionKey {
		t.Fatalf("stable identity did not continue: %q/%q errors=%v/%v", o1.SessionKey, o2.SessionKey, err1, err2)
	}
	rA := httptest.NewRequest("POST", "/v1/responses", nil)
	rA.Header.Set("Authorization", "Bearer key-a")
	rB := httptest.NewRequest("POST", "/v1/responses", nil)
	rB.Header.Set("Authorization", "Bearer key-b")
	a := scopedSessionKey(rA, o1.SessionKey)
	if a == "" || a == scopedSessionKey(rB, o1.SessionKey) || a != scopedSessionKey(rA, o2.SessionKey) {
		t.Fatal("credential-scoped continuation is unstable or colliding")
	}
	xA := httptest.NewRequest("POST", "/v1/responses", nil)
	xA.Header.Set("X-API-Key", "key-a")
	xB := httptest.NewRequest("POST", "/v1/responses", nil)
	xB.Header.Set("X-API-Key", "key-b")
	if scopedSessionKey(xA, o1.SessionKey) == scopedSessionKey(xB, o1.SessionKey) {
		t.Fatal("X-API-Key sessions crossed API credentials")
	}
	if scopedSessionKey(rA, o1.SessionKey) != scopedSessionKey(xA, o1.SessionKey) {
		t.Fatal("the same credential changed session scope across supported auth headers")
	}
}

func TestResponsesPromptCacheKeyDoesNotCreateConversationIdentity(t *testing.T) {
	o1, err1 := (responsesRequest{Input: "one", PromptCacheKey: "shared-cache-bucket"}).openAI()
	o2, err2 := (responsesRequest{Input: "unrelated", PromptCacheKey: "shared-cache-bucket"}).openAI()
	if err1 != nil || err2 != nil {
		t.Fatalf("prompt cache requests failed conversion: %v/%v", err1, err2)
	}
	if o1.SessionKey != "" || o2.SessionKey != "" {
		t.Fatalf("prompt_cache_key incorrectly became conversation identity: %q/%q", o1.SessionKey, o2.SessionKey)
	}
}

func TestResponsesAcceptsTextVerbosity(t *testing.T) {
	r := responsesRequest{Input: "hi", Text: map[string]any{"verbosity": "low"}}
	if err := r.validateSemantics(map[string]json.RawMessage{"text": nil}); err != nil {
		t.Fatalf("text.verbosity must be accepted and ignored: %v", err)
	}
	if _, err := r.openAI(); err != nil {
		t.Fatalf("text.verbosity request failed conversion: %v", err)
	}
}

func TestResponsesMergesAdditionalToolsIntoUpstreamSchema(t *testing.T) {
	r := responsesRequest{Input: []any{
		map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{map[string]any{
			"type": "namespace", "name": "functions", "description": "", "tools": []any{
				map[string]any{"type": "custom", "name": "exec", "description": "run js"},
				map[string]any{"type": "custom", "name": "apply_patch", "description": "edit files"},
			},
		}}},
		map[string]any{"type": "message", "role": "user", "content": "分析下当前项目"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatalf("additional_tools item must merge, not reject: %v", err)
	}
	if len(o.Messages) != 1 || o.Messages[0].Role != "user" {
		t.Fatalf("additional_tools leaked into conversation history: %#v", o.Messages)
	}
	names := map[string]bool{}
	for _, tool := range o.Tools {
		var f struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(tool.Function, &f) == nil && f.Name != "" {
			names[f.Name] = true
		}
	}
	for _, want := range []string{"functions.exec", "functions.apply_patch"} {
		if !names[want] {
			t.Fatalf("additional_tools tool %q missing from upstream schema: %v", want, names)
		}
	}
}
