package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

type gatewayHubOutcome int

const (
	gatewayHubSuccess gatewayHubOutcome = iota
	gatewayHubRateLimited
	gatewayHubPartialThenDrop
	gatewayHubToolDecision
	gatewayHubDiagnosticDecision
)

type scriptedGatewayHub struct {
	mu       sync.Mutex
	calls    []string
	counts   map[string]int
	payloads []string
	script   func(token string, accountCall int) gatewayHubOutcome
}

func (h *scriptedGatewayHub) record(token string) (int, gatewayHubOutcome) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, token)
	h.counts[token]++
	n := h.counts[token]
	return n, h.script(token, n)
}

func (h *scriptedGatewayHub) snapshot() ([]string, map[string]int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	calls := append([]string(nil), h.calls...)
	counts := make(map[string]int, len(h.counts))
	for key, value := range h.counts {
		counts[key] = value
	}
	return calls, counts
}

func (h *scriptedGatewayHub) recordPayload(payload string) {
	h.mu.Lock()
	h.payloads = append(h.payloads, payload)
	h.mu.Unlock()
}

func (h *scriptedGatewayHub) payloadSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.payloads...)
}

func writeGatewaySignalR(t *testing.T, conn *websocket.Conn, frames ...map[string]any) {
	t.Helper()
	var payload strings.Builder
	for _, frame := range frames {
		encoded, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		payload.Write(encoded)
		payload.WriteByte(0x1e)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload.String())); err != nil {
		t.Errorf("write SignalR: %v", err)
	}
}

func newScriptedGatewayServer(t *testing.T, script func(string, int) gatewayHubOutcome, accountIDs ...string) (*Server, *scriptedGatewayHub) {
	t.Helper()
	hub := &scriptedGatewayHub{counts: map[string]int{}, script: script}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("access_token")
		_, outcome := hub.record(token)
		if outcome == gatewayHubRateLimited {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("{}\x1e")); err != nil {
			return
		}
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		hub.recordPayload(string(payload))
		if outcome == gatewayHubPartialThenDrop {
			writeGatewaySignalR(t, conn, map[string]any{
				"type":   1,
				"target": "update",
				"arguments": []any{map[string]any{
					"writeAtCursor": "partial-before-drop",
				}},
			})
			return
		}
		responseText := "ok-" + token
		if outcome == gatewayHubToolDecision {
			responseText = `{"calls":[]}`
		}
		if outcome == gatewayHubDiagnosticDecision {
			responseText = `{"calls":[{"name":"inspect","arguments":{"path":"logs"}}]}`
		}
		writeGatewaySignalR(t, conn,
			map[string]any{
				"type":   1,
				"target": "update",
				"arguments": []any{map[string]any{
					"writeAtCursor": responseText,
				}},
			},
			map[string]any{"type": 3},
		)
	}))
	t.Cleanup(upstream.Close)

	server := newStickyAccountTestServer(t, accountIDs...)
	client := chathub.NewClient()
	client.WSBase = "ws" + strings.TrimPrefix(upstream.URL, "http")
	server.chat = client
	server.sessions = &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	server.settings = &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), v: defaultRuntimeSettings()}
	return server, hub
}

func gatewayNativeChat(t *testing.T, server *Server, message string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"message": message})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	server.chatOnce(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode chat response: %v body=%s", err, recorder.Body.String())
	}
	return result.Account.ID
}

func TestGatewayCallsOnlyCurrentAccountWhileHealthy(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(string, int) gatewayHubOutcome {
		return gatewayHubSuccess
	}, "account-1", "account-2", "account-3", "account-4")

	for i := 0; i < 12; i++ {
		if account := gatewayNativeChat(t, server, fmt.Sprintf("request-%d", i)); account != "account-1" {
			t.Fatalf("request %d used %s; want account-1", i, account)
		}
	}
	calls, counts := hub.snapshot()
	if len(calls) != 12 || counts["token-account-1"] != 12 {
		t.Fatalf("unexpected upstream calls=%v counts=%v", calls, counts)
	}
	for _, token := range []string{"token-account-2", "token-account-3", "token-account-4"} {
		if counts[token] != 0 {
			t.Fatalf("non-current %s received %d upstream calls", token, counts[token])
		}
	}
}

func TestGatewayFailoverOrderIsStrictOneTwoThreeFour(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(token string, accountCall int) gatewayHubOutcome {
		switch token {
		case "token-account-1":
			return gatewayHubRateLimited
		case "token-account-2", "token-account-3":
			if accountCall == 2 {
				return gatewayHubRateLimited
			}
		}
		return gatewayHubSuccess
	}, "account-1", "account-2", "account-3", "account-4")

	wantServing := []string{"account-2", "account-3", "account-4", "account-4"}
	for i, want := range wantServing {
		if got := gatewayNativeChat(t, server, fmt.Sprintf("turn-%d", i+1)); got != want {
			t.Fatalf("turn %d served by %s; want %s", i+1, got, want)
		}
	}
	calls, _ := hub.snapshot()
	wantCalls := []string{
		"token-account-1", "token-account-2",
		"token-account-2", "token-account-3",
		"token-account-3", "token-account-4",
		"token-account-4",
	}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("upstream order=%v want=%v", calls, wantCalls)
	}
}

func TestFailureBeforeOutputRetriesExactlyNextAccount(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(token string, _ int) gatewayHubOutcome {
		if token == "token-account-1" {
			return gatewayHubRateLimited
		}
		return gatewayHubSuccess
	}, "account-1", "account-2", "account-3")

	if got := gatewayNativeChat(t, server, "pre-output failure"); got != "account-2" {
		t.Fatalf("served by %s; want account-2", got)
	}
	calls, counts := hub.snapshot()
	if strings.Join(calls, ",") != "token-account-1,token-account-2" {
		t.Fatalf("pre-output retry order=%v", calls)
	}
	if counts["token-account-3"] != 0 {
		t.Fatalf("request skipped past account-2 to account-3: %v", counts)
	}

	if got := gatewayNativeChat(t, server, "sticky after recovery"); got != "account-2" {
		t.Fatalf("next request served by %s; want account-2", got)
	}
	calls, _ = hub.snapshot()
	if strings.Join(calls, ",") != "token-account-1,token-account-2,token-account-2" {
		t.Fatalf("post-failover stickiness calls=%v", calls)
	}
}

func TestStreamingToolRouterFailoverUsesReplacementLeaseContext(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(token string, accountCall int) gatewayHubOutcome {
		if token == "token-account-1" {
			return gatewayHubRateLimited
		}
		if token == "token-account-2" && accountCall == 1 {
			return gatewayHubToolDecision
		}
		return gatewayHubSuccess
	}, "account-1", "account-2", "account-3")

	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"inspect the project"}],"stream":true,"tools":[{"type":"function","function":{"name":"inspect","description":"inspect","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],"tool_choice":"auto"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	server.openaiChat(recorder, req)
	stream := recorder.Body.String()
	if strings.Contains(stream, "operation was canceled") || strings.Contains(stream, "upstream_error") || !strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("tool-router failover did not recover: %s", stream)
	}
	calls, counts := hub.snapshot()
	if strings.Join(calls, ",") != "token-account-1,token-account-2,token-account-2" || counts["token-account-3"] != 0 {
		t.Fatalf("replacement lease was not used deterministically: calls=%v counts=%v", calls, counts)
	}
}

func TestRepeatedToolFailureRoutesToDifferentDiagnosticCall(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(string, int) gatewayHubOutcome {
		return gatewayHubDiagnosticDecision
	}, "account-1", "account-2")
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"messages":[
			{"role":"user","content":"build the project"},
			{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"run","arguments":"{\"cmd\":\"build\"}"}}]},
			{"role":"tool","tool_call_id":"a","content":"exit code 1: build failed"},
			{"role":"assistant","tool_calls":[{"id":"b","type":"function","function":{"name":"run","arguments":"{\"cmd\":\"build\"}"}}]},
			{"role":"tool","tool_call_id":"b","content":"exit code 1: build failed"}
		],
		"tools":[
			{"type":"function","function":{"name":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}},
			{"type":"function","function":{"name":"inspect","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}
		],
		"tool_choice":"auto"
	}`)
	recorder := httptest.NewRecorder()
	server.openaiChat(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"inspect"`) || strings.Contains(recorder.Body.String(), "repeated tool failure") {
		t.Fatalf("repeated failure did not recover through a different diagnostic call: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	calls, _ := hub.snapshot()
	if strings.Join(calls, ",") != "token-account-1" {
		t.Fatalf("unexpected upstream calls: %v", calls)
	}
}

func TestFailureAfterFirstOutputResumesSameConversationWithoutAccountReplay(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(token string, accountCall int) gatewayHubOutcome {
		if token == "token-account-1" && accountCall == 1 {
			return gatewayHubPartialThenDrop
		}
		return gatewayHubSuccess
	}, "account-1", "account-2", "account-3")

	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"stream once"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	server.openaiChat(recorder, req)
	stream := recorder.Body.String()
	if !strings.Contains(stream, "partial-before-drop") {
		t.Fatalf("first visible delta missing: %s", stream)
	}
	if !strings.Contains(stream, "ok-token-account-1") {
		t.Fatalf("same-account recovery text missing: %s", stream)
	}
	if strings.Contains(stream, "upstream_error") || !strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("recovered partial stream did not complete cleanly: %s", stream)
	}
	calls, counts := hub.snapshot()
	if strings.Join(calls, ",") != "token-account-1,token-account-1" || counts["token-account-2"] != 0 {
		t.Fatalf("visible response was not resumed on the same account; calls=%v counts=%v", calls, counts)
	}
	payloads := hub.payloadSnapshot()
	if len(payloads) != 2 || !strings.Contains(payloads[1], "ALREADY_DELIVERED_ASSISTANT_TAIL") || !strings.Contains(payloads[1], "partial-before-drop") {
		t.Fatalf("recovery prompt lost partial evidence: %v", payloads)
	}

	if got := gatewayNativeChat(t, server, "next request"); got != "account-1" {
		t.Fatalf("request after recovered transient used %s; want account-1", got)
	}
	calls, _ = hub.snapshot()
	if strings.Join(calls, ",") != "token-account-1,token-account-1,token-account-1" {
		t.Fatalf("next-request order=%v", calls)
	}
}

func TestRepeatedVisibleStreamFailureAdvancesOnlyFollowingRequest(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(token string, _ int) gatewayHubOutcome {
		if token == "token-account-1" {
			return gatewayHubPartialThenDrop
		}
		return gatewayHubSuccess
	}, "account-1", "account-2", "account-3")
	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"stream once"}],"stream":true}`)
	recorder := httptest.NewRecorder()
	server.openaiChat(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))
	if stream := recorder.Body.String(); !strings.Contains(stream, "upstream_error") || !strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("repeated partial failure lacked explicit terminal: %s", stream)
	}
	calls, counts := hub.snapshot()
	if strings.Join(calls, ",") != "token-account-1,token-account-1" || counts["token-account-2"] != 0 {
		t.Fatalf("recovery swept into another account: calls=%v counts=%v", calls, counts)
	}
	if got := gatewayNativeChat(t, server, "next request"); got != "account-2" {
		t.Fatalf("following request used %s; want account-2", got)
	}
}

func TestConcurrentMixedFailuresAdvanceExactlyOneSlot(t *testing.T) {
	server := newStickyAccountTestServer(t, "account-1", "account-2", "account-3", "account-4")
	first, err := server.resolveAccount("")
	if err != nil || first.ID != "account-1" {
		t.Fatalf("initial account=%s err=%v", first.ID, err)
	}
	failures := []error{
		&chathub.RateLimitError{StatusCode: http.StatusTooManyRequests, Reason: "rate limited"},
		errors.New("HTTP 503 service unavailable"),
		errors.New("ws read before completion: unexpected EOF"),
		errors.New("proxy dialer: connection refused"),
		context.DeadlineExceeded,
	}
	var wait sync.WaitGroup
	for i := 0; i < 100; i++ {
		wait.Add(1)
		go func(failure error) {
			defer wait.Done()
			server.markAccountResult(first.ID, failure)
		}(failures[i%len(failures)])
	}
	wait.Wait()

	if active := server.currentActiveAccountID(); active != "account-2" {
		t.Fatalf("concurrent failures advanced to %s; want account-2", active)
	}
	if next, err := server.resolveAccount(""); err != nil || next.ID != "account-2" {
		t.Fatalf("next account=%s err=%v; want account-2", next.ID, err)
	}
}

func TestConcurrentHandlerFailuresDoNotStampedeReplacementAccount(t *testing.T) {
	const workers = 50
	var retiredCalls atomic.Int32
	var replacementCalls atomic.Int32
	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("access_token") {
		case "token-account-1":
			if retiredCalls.Add(1) == workers {
				close(release)
			}
			select {
			case <-release:
			case <-time.After(5 * time.Second):
				t.Error("not all requests reached the retired account barrier")
			}
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		case "token-account-2":
			replacementCalls.Add(1)
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte("{}\x1e")); err != nil {
				return
			}
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			writeGatewaySignalR(t, conn,
				map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"writeAtCursor": "ok"}}},
				map[string]any{"type": 3},
			)
		default:
			http.Error(w, "unexpected account", http.StatusBadRequest)
		}
	}))
	defer upstream.Close()

	server := newStickyAccountTestServer(t, "account-1", "account-2", "account-3")
	client := chathub.NewClient()
	client.WSBase = "ws" + strings.TrimPrefix(upstream.URL, "http")
	server.chat = client
	server.sessions = &sessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), data: map[string]conversation{}}
	server.settings = &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), v: defaultRuntimeSettings()}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"message":"barrier"}`))
			server.chatOnce(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()
	if got := retiredCalls.Load(); got != workers {
		t.Fatalf("retired account calls=%d want=%d", got, workers)
	}
	if got := replacementCalls.Load(); got != 1 {
		t.Fatalf("replacement account immediate retries=%d want=1", got)
	}
}

func TestStaleFormerAccountFailureCannotRewindActiveOrder(t *testing.T) {
	server := newStickyAccountTestServer(t, "account-1", "account-2", "account-3", "account-4")
	first, err := server.resolveAccount("")
	if err != nil || first.ID != "account-1" {
		t.Fatalf("initial account=%s err=%v", first.ID, err)
	}
	// Use an account-scoped proxy failure here so this test isolates the route
	// compare-and-swap invariant from the shared-upstream circuit breaker.
	failure := errors.New("proxy dialer: connection refused")

	// Request A fails on account-1 and moves the active slot to account-2.
	server.markAccountResult("account-1", failure)
	if active, err := server.resolveAccount(""); err != nil || active.ID != "account-2" {
		t.Fatalf("after account-1 failure active=%s err=%v; want account-2", active.ID, err)
	}

	// A newer request then fails on account-2 and correctly advances to account-3.
	server.markAccountResult("account-2", failure)
	if active, err := server.resolveAccount(""); err != nil || active.ID != "account-3" {
		t.Fatalf("after account-2 failure active=%s err=%v; want account-3", active.ID, err)
	}

	// A delayed duplicate error from the former account-1 must be ignored; only
	// a compare-and-swap failure from the current identity may advance routing.
	server.markAccountResult("account-1", failure)
	if active, err := server.resolveAccount(""); err != nil || active.ID != "account-3" {
		t.Fatalf("stale account-1 failure rewound active=%s err=%v; want account-3", active.ID, err)
	}
}

func TestInactiveAccountIsolationBlocksChatImageAndRefreshWithoutUpstream(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(string, int) gatewayHubOutcome {
		return gatewayHubSuccess
	}, "account-1", "account-2", "account-3")

	chatRequest := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"message":"must stay isolated","accountId":"account-2"}`))
	chatResponse := httptest.NewRecorder()
	server.chatOnce(chatResponse, chatRequest)
	if chatResponse.Code != http.StatusBadRequest {
		t.Fatalf("inactive chat account status=%d body=%s; want 400", chatResponse.Code, chatResponse.Body.String())
	}
	compatRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"must stay isolated"}],"accountId":"account-2"}`))
	compatResponse := httptest.NewRecorder()
	server.openaiChat(compatResponse, compatRequest)
	if compatResponse.Code != http.StatusBadRequest {
		t.Fatalf("inactive OpenAI account status=%d body=%s; want 400", compatResponse.Code, compatResponse.Body.String())
	}

	imageRequest := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"must stay isolated","accountId":"account-2"}`))
	imageResponse := httptest.NewRecorder()
	server.imageGenerations(imageResponse, imageRequest)
	if imageResponse.Code != http.StatusConflict || !strings.Contains(imageResponse.Body.String(), "account_isolated") {
		t.Fatalf("inactive image account status=%d body=%s; want account_isolated 409", imageResponse.Code, imageResponse.Body.String())
	}

	refreshRequest := httptest.NewRequest(http.MethodPost, "/api/accounts/refresh", strings.NewReader(`{"id":"account-2"}`))
	refreshResponse := httptest.NewRecorder()
	server.refreshAccount(refreshResponse, refreshRequest)
	if refreshResponse.Code != http.StatusConflict || !strings.Contains(refreshResponse.Body.String(), "account_isolated") {
		t.Fatalf("inactive refresh account status=%d body=%s; want account_isolated 409", refreshResponse.Code, refreshResponse.Body.String())
	}

	if calls, counts := hub.snapshot(); len(calls) != 0 || counts["token-account-2"] != 0 {
		t.Fatalf("isolated account reached upstream: calls=%v counts=%v", calls, counts)
	}
	if active := server.currentActiveAccountID(); active != "account-1" {
		t.Fatalf("isolation request changed active account to %s; want account-1", active)
	}
}

func TestActiveAccountPersistsAcrossRestart(t *testing.T) {
	routePath := filepath.Join(t.TempDir(), "account-route.json")
	firstServer := newStickyAccountTestServer(t, "account-1", "account-2", "account-3")
	firstServer.accountRoutePath = routePath
	if err := firstServer.initializeAccountRouter(); err != nil {
		t.Fatalf("initialize first router: %v", err)
	}
	if active := firstServer.currentActiveAccountID(); active != "account-1" {
		t.Fatalf("initial active account=%s; want account-1", active)
	}
	firstServer.markAccountResult("account-1", errors.New("HTTP 503 service unavailable"))
	if active := firstServer.currentActiveAccountID(); active != "account-2" {
		t.Fatalf("active account after failure=%s; want account-2", active)
	}

	// Recreate the server and token store while reusing only the persisted
	// router state, matching a process/container restart.
	restarted := newStickyAccountTestServer(t, "account-1", "account-2", "account-3")
	restarted.accountRoutePath = routePath
	if err := restarted.initializeAccountRouter(); err != nil {
		t.Fatalf("initialize restarted router: %v", err)
	}
	if active := restarted.currentActiveAccountID(); active != "account-2" {
		t.Fatalf("restart restored active account=%s; want account-2", active)
	}
	if restarted.healthPool().Available("account-1") {
		t.Fatal("restart lost account-1 transient isolation state")
	}
	if resolved, err := restarted.resolveAccount(""); err != nil || resolved.ID != "account-2" {
		t.Fatalf("restart resolved account=%s err=%v; want account-2", resolved.ID, err)
	}
	restarted.markAccountResult("account-2", &chathub.RateLimitError{StatusCode: http.StatusTooManyRequests, Reason: "rate limited"})
	if active := restarted.currentActiveAccountID(); active != "account-3" {
		t.Fatalf("after account-2 failure active=%s; want account-3", active)
	}
	restarted.markAccountResult("account-3", &chathub.RateLimitError{StatusCode: http.StatusTooManyRequests, Reason: "rate limited"})
	if active := restarted.currentActiveAccountID(); active != "account-3" {
		t.Fatalf("persisted cooldown was bypassed after wrap: active=%s; want account-3 held", active)
	}
}

func TestSuccessfulQuotaBoundaryDoesNotQuarantineOrRotate(t *testing.T) {
	server := newStickyAccountTestServer(t, "account-1", "account-2")
	server.markAccountSuccess("account-1", map[string]any{"CostQuota": float64(0)})
	if !server.healthPool().Available("account-1") {
		t.Fatal("complete successful response incorrectly quarantined account-1")
	}
	resolved, err := server.resolveAccount("")
	if err != nil || resolved.ID != "account-1" {
		t.Fatalf("resolved=%s error=%v; complete success must stay on account-1", resolved.ID, err)
	}
}

func TestFailoverRebuildPreservesMultiRoundToolOrder(t *testing.T) {
	messages := []oaiMsg{
		{Role: "user", Content: "ORDER_USER_INITIAL"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_first", "type": "function", "function": map[string]any{"name": "first_tool", "arguments": `{"step":1}`}}}},
		{Role: "tool", ToolCallID: "call_first", Content: "ORDER_RESULT_FIRST"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_second", "type": "function", "function": map[string]any{"name": "second_tool", "arguments": `{"step":2}`}}}},
		{Role: "tool", ToolCallID: "call_second", Content: "ORDER_RESULT_SECOND"},
		{Role: "user", Content: "ORDER_USER_FINAL"},
	}
	prompt, ok := rebuildFullHistoryPrompt(messages, "gpt-5.6-sol", nil, nil, "", streamAnswerRule)
	if !ok {
		t.Fatal("failed to rebuild bounded full history")
	}
	markers := []string{
		"ORDER_USER_INITIAL", "first_tool", "call_first", "ORDER_RESULT_FIRST",
		"second_tool", "call_second", "ORDER_RESULT_SECOND", "ORDER_USER_FINAL",
	}
	last := -1
	for _, marker := range markers {
		position := strings.Index(prompt, marker)
		if position < 0 {
			t.Fatalf("rebuilt prompt missing %q: %s", marker, prompt)
		}
		if position <= last {
			t.Fatalf("marker %q is out of order in rebuilt prompt: %s", marker, prompt)
		}
		last = position
	}
}

func TestResponsesPreviousResponseFailoverRestoresToolContext(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(string, int) gatewayHubOutcome {
		return gatewayHubSuccess
	}, "account-1", "account-2", "account-3")

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer portable-test-key")
	previousID := "resp_previous"
	source := scopedSessionKey(request, responseIDSessionKey(previousID))
	portable := []oaiMsg{
		{Role: "user", Content: "PREVIOUS_USER_GOAL"},
		{Role: "assistant", ToolCalls: []map[string]any{{
			"id": "call_previous", "type": "function",
			"function": map[string]any{"name": "inspect", "arguments": `{"path":"a"}`},
		}}},
	}
	if _, err := server.sessions.upsert(conversation{
		ID: source, AccountID: "account-1", ConversationID: "old-conversation",
		SessionID: "old-session", PortableMessages: portable,
	}); err != nil {
		t.Fatal(err)
	}
	server.markAccountResult("account-1", errors.New("HTTP 503 service unavailable"))
	if active := server.currentActiveAccountID(); active != "account-2" {
		t.Fatalf("active account=%s; want account-2", active)
	}

	body := map[string]any{
		"model": "gpt-5.6-sol", "previous_response_id": previousID,
		"input": []any{map[string]any{
			"type": "function_call_output", "call_id": "call_previous", "output": "PREVIOUS_TOOL_RESULT",
		}},
	}
	encoded, _ := json.Marshal(body)
	request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer portable-test-key")
	recorder := httptest.NewRecorder()
	server.responses(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("responses status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	calls, counts := hub.snapshot()
	if strings.Join(calls, ",") != "token-account-2" || counts["token-account-1"] != 0 || counts["token-account-3"] != 0 {
		t.Fatalf("previous_response_id woke isolated account: calls=%v counts=%v", calls, counts)
	}
	payloads := hub.payloadSnapshot()
	if len(payloads) != 1 {
		t.Fatalf("upstream payload count=%d", len(payloads))
	}
	prompt := payloads[0]
	markers := []string{"PREVIOUS_USER_GOAL", "[assistant tool_calls]", "inspect", "call_previous", "PREVIOUS_TOOL_RESULT"}
	last := -1
	for _, marker := range markers {
		position := strings.Index(prompt, marker)
		if position < 0 || position <= last {
			t.Fatalf("Responses failover context missing or reordered at %q: %s", marker, prompt)
		}
		last = position
	}
	parent, ok := server.sessions.get(source)
	if !ok || parent.AccountID != "account-1" || parent.ConversationID != "old-conversation" || parent.SessionID != "old-session" {
		t.Fatalf("immutable previous_response_id source was mutated: %+v ok=%v", parent, ok)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	responseID, _ := response["id"].(string)
	target := scopedSessionKey(request, responseIDSessionKey(responseID))
	rebound, ok := server.sessions.get(target)
	if !ok || rebound.AccountID != "account-2" || rebound.ConversationID == "old-conversation" || rebound.SessionID == "old-session" {
		t.Fatalf("response target not rebound to active account: %+v ok=%v", rebound, ok)
	}
	if len(rebound.PortableMessages) < 4 {
		t.Fatalf("response target history not extended with tool result and assistant answer: %+v", rebound.PortableMessages)
	}
}

func TestLegacyStreamFailureSwitchesAtMostOncePerRequest(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(string, int) gatewayHubOutcome {
		return gatewayHubRateLimited
	}, "account-1", "account-2", "account-3")

	body, _ := json.Marshal(map[string]any{"message": "single-switch-boundary"})
	req := httptest.NewRequest(http.MethodPost, "/api/chat/stream", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	server.chatStream(recorder, req)

	if active := server.activeAccountID; active != "account-2" {
		t.Fatalf("one request advanced beyond its single-switch budget: active=%s", active)
	}
	calls, counts := hub.snapshot()
	if strings.Join(calls, ",") != "token-account-1,token-account-2" || counts["token-account-3"] != 0 {
		t.Fatalf("unexpected account calls=%v counts=%v", calls, counts)
	}
	if !strings.Contains(recorder.Body.String(), "upstream_error") {
		t.Fatalf("missing explicit terminal error: %s", recorder.Body.String())
	}
}

func TestImageFailureSwitchesAtMostOncePerRequest(t *testing.T) {
	server, hub := newScriptedGatewayServer(t, func(string, int) gatewayHubOutcome {
		return gatewayHubRateLimited
	}, "account-1", "account-2", "account-3")

	body, _ := json.Marshal(map[string]any{"prompt": "single-switch-boundary", "n": 1})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	server.imageGenerations(recorder, req)

	if active := server.activeAccountID; active != "account-2" {
		t.Fatalf("one image request advanced beyond its single-switch budget: active=%s", active)
	}
	calls, counts := hub.snapshot()
	if strings.Join(calls, ",") != "token-account-1,token-account-2" || counts["token-account-3"] != 0 {
		t.Fatalf("unexpected account calls=%v counts=%v", calls, counts)
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestLegacyStreamPersistenceFailureIsNotReportedDone(t *testing.T) {
	server, _ := newScriptedGatewayServer(t, func(string, int) gatewayHubOutcome {
		return gatewayHubSuccess
	}, "account-1", "account-2")
	server.sessions.path = string([]byte{0})

	body, _ := json.Marshal(map[string]any{"message": "persist-boundary", "sessionKey": "thread-1"})
	req := httptest.NewRequest(http.MethodPost, "/api/chat/stream", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	server.chatStream(recorder, req)

	responseBody := recorder.Body.String()
	if !strings.Contains(responseBody, "session_persistence_error") {
		t.Fatalf("missing persistence error: %s", responseBody)
	}
	if strings.Contains(responseBody, "event: done") {
		t.Fatalf("persistence failure was falsely reported done: %s", responseBody)
	}
}

func TestChatCompletionPersistenceFailureIsNotReportedSuccessful(t *testing.T) {
	server, _ := newScriptedGatewayServer(t, func(string, int) gatewayHubOutcome {
		return gatewayHubSuccess
	}, "account-1", "account-2")
	server.sessions.path = string([]byte{0})

	body, _ := json.Marshal(map[string]any{
		"model":       "gpt-5.6-sol",
		"session_key": "thread-1",
		"messages":    []any{map[string]any{"role": "user", "content": "persist-boundary"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	server.openaiChat(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "session_persistence_error") {
		t.Fatalf("missing persistence error: %s", recorder.Body.String())
	}
}
