package chathub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testChatHubServer(t *testing.T, script func(*websocket.Conn)) (*httptest.Server, *Client) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("{}"+rs)); err != nil {
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		script(conn)
	}))
	client := NewClient()
	client.WSBase = "ws" + strings.TrimPrefix(server.URL, "http")
	return server, client
}

func testAccount() Account {
	return Account{AccessToken: "token", OID: "oid", TID: "tid"}
}

func writeSignalR(t *testing.T, conn *websocket.Conn, frames ...map[string]any) {
	t.Helper()
	var payload strings.Builder
	for _, frame := range frames {
		b, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		payload.Write(b)
		payload.WriteString(rs)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload.String())); err != nil {
		t.Errorf("write SignalR: %v", err)
	}
}

func TestChatPreservesWhitespaceDeltas(t *testing.T) {
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		writeSignalR(t, conn,
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"writeAtCursor": "Hello"}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"writeAtCursor": " "}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"writeAtCursor": "world"}}},
			map[string]any{"type": 3},
		)
	})
	defer server.Close()

	var deltas strings.Builder
	res, err := client.ChatWithDelta(context.Background(), testAccount(), Request{Text: "hi"}, func(delta string) error {
		deltas.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := deltas.String(); got != "Hello world" {
		t.Fatalf("deltas=%q", got)
	}
	if res.Text != "Hello world" {
		t.Fatalf("result=%q", res.Text)
	}
}

func TestChatReplaysLiveMemoryUpdateAndAnswerCursorOnSeparateTracks(t *testing.T) {
	const answerID = "9ab0502f-3d83-46e7-8a5c-97ed8a6b0156"
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		memory := map[string]any{
			"text": "Remember marker CONTEXT_LINK_OK", "hiddenText": "Remember marker CONTEXT_LINK_OK",
			"author": "bot", "messageId": "memory-message", "messageType": "MemoryUpdate",
		}
		writeSignalR(t, conn,
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"messages": []any{memory}}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"messages": []any{map[string]any{
				"text": memory["text"], "hiddenText": memory["hiddenText"], "author": "bot", "messageId": "memory-message",
				"messageType": "MemoryUpdate", "invocation": `[{"function":{"name":"record_memory"}}]`,
				"pluginInfo": map[string]any{"id": "UpdateMemory"},
			}}}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{
				"cursor":   map[string]any{"j": "$['" + answerID + "'].adaptiveCards[0].body[0].text", "p": -1},
				"messages": []any{map[string]any{"text": "SM", "author": "bot", "messageId": answerID}},
			}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"writeAtCursor": "OKE_OK"}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{
				"messages": []any{map[string]any{"text": "SMOKE_OK", "author": "bot", "messageId": answerID}}, "isLastUpdate": true,
			}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{
				"messages": []any{map[string]any{"text": "SMOKE_OK", "author": "bot", "messageId": answerID}},
			}}},
			map[string]any{"type": 2, "item": map[string]any{"result": map[string]any{"value": "Success", "message": "SMOKE_OK"}}},
			map[string]any{"type": 3},
		)
	})
	defer server.Close()

	var streamed strings.Builder
	res, err := client.ChatWithDelta(context.Background(), testAccount(), Request{Text: "remember then answer"}, func(delta string) error {
		streamed.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := streamed.String(); got != "SMOKE_OK" {
		t.Fatalf("internal MemoryUpdate leaked or cursor was spliced: %q", got)
	}
	if res.Text != "SMOKE_OK" || res.FullText != "SMOKE_OK" {
		t.Fatalf("terminal authority lost: text=%q full=%q", res.Text, res.FullText)
	}
}

func TestChatNonPrefixFinalSnapshotIsAuthoritative(t *testing.T) {
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		writeSignalR(t, conn,
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"messages": []any{map[string]any{"text": "draft answer", "author": "bot", "messageId": "answer-1"}}}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"messages": []any{map[string]any{"text": "rewritten final", "author": "bot", "messageId": "answer-1"}}, "isLastUpdate": true}}},
			map[string]any{"type": 3},
		)
	})
	defer server.Close()

	var streamed strings.Builder
	res, err := client.ChatWithDelta(context.Background(), testAccount(), Request{Text: "rewrite"}, func(delta string) error {
		streamed.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := streamed.String(); got != "draft answer" {
		t.Fatalf("non-prefix rewrite was incorrectly appended: %q", got)
	}
	if res.Text != "rewritten final" || res.FullText != "rewritten final" {
		t.Fatalf("final snapshot did not replace draft: text=%q full=%q", res.Text, res.FullText)
	}
}

func TestChatRepeatedSnapshotsDoNotDuplicateVisibleText(t *testing.T) {
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		initial := map[string]any{"text": "Hello", "author": "bot", "messageId": "answer-repeat"}
		writeSignalR(t, conn,
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"cursor": map[string]any{"j": "$['answer-repeat'].adaptiveCards[0].body[0].text"}, "messages": []any{initial}}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"messages": []any{initial}}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"writeAtCursor": " world"}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"messages": []any{map[string]any{"text": "Hello world", "author": "bot", "messageId": "answer-repeat"}}, "isLastUpdate": true}}},
			map[string]any{"type": 3},
		)
	})
	defer server.Close()

	var streamed strings.Builder
	res, err := client.ChatWithDelta(context.Background(), testAccount(), Request{Text: "repeat"}, func(delta string) error {
		streamed.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "Hello world" || res.Text != "Hello world" || res.FullText != "Hello world" {
		t.Fatalf("duplicate snapshot merge: stream=%q text=%q full=%q", streamed.String(), res.Text, res.FullText)
	}
}

func TestChatMixedProgressDoesNotSuppressActiveAnswerCursor(t *testing.T) {
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		writeSignalR(t, conn,
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{
				"cursor":   map[string]any{"j": "$['mixed-answer'].adaptiveCards[0].body[0].text"},
				"messages": []any{map[string]any{"text": "Hello", "author": "bot", "messageId": "mixed-answer"}},
			}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{
				"messages":      []any{map[string]any{"text": "searching", "author": "bot", "messageType": "Progress", "contentType": "SearchResults"}},
				"writeAtCursor": " world",
			}}},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"messages": []any{map[string]any{"text": "Hello world", "author": "bot", "messageId": "mixed-answer"}}, "isLastUpdate": true}}},
			map[string]any{"type": 3},
		)
	})
	defer server.Close()

	var streamed strings.Builder
	res, err := client.ChatWithDelta(context.Background(), testAccount(), Request{Text: "mixed"}, func(delta string) error {
		streamed.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "Hello world" || res.Text != "Hello world" {
		t.Fatalf("progress suppressed or contaminated answer: stream=%q text=%q", streamed.String(), res.Text)
	}
}

func TestChatSendsSignalRKeepalive(t *testing.T) {
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("waiting for ping: %v", err)
				return
			}
			if strings.Contains(string(raw), `"type":6`) {
				writeSignalR(t, conn,
					map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"writeAtCursor": "ok"}}},
					map[string]any{"type": 3},
				)
				return
			}
		}
	})
	defer server.Close()
	client.PingInterval = 20 * time.Millisecond

	res, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "ok" {
		t.Fatalf("result=%q", res.Text)
	}
}

func TestChatRejectsSignalRTerminalErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame map[string]any
		want  string
	}{
		{name: "completion string error", frame: map[string]any{"type": 3, "error": "backend failed"}, want: "backend failed"},
		{name: "close frame", frame: map[string]any{"type": 7, "error": "server timeout"}, want: "server timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			server, client := testChatHubServer(t, func(conn *websocket.Conn) {
				attempts.Add(1)
				writeSignalR(t, conn, tc.frame)
			})
			defer server.Close()

			_, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v", err)
			}
			if attempts.Load() != 1 {
				t.Fatalf("terminal upstream error replayed on the same account: attempts=%d", attempts.Load())
			}
		})
	}
}

func TestChatCancellationInterruptsRead(t *testing.T) {
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage()
	})
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := client.Chat(ctx, testAccount(), Request{Text: "hi"})
	if err == nil {
		t.Fatal("expected cancellation")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func TestChatDoesNotRetryPartialOutputWhenCallerHasNotCommittedIt(t *testing.T) {
	var attempts atomic.Int32
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		attempts.Add(1)
		writeSignalR(t, conn, map[string]any{
			"type":   1,
			"target": "update",
			"arguments": []any{map[string]any{
				"writeAtCursor": "unsafe partial instruction",
			}},
		})
		// Returning closes the socket without a SignalR type-3 completion. The
		// request payload has already been sent, so even buffered output must not
		// make this exchange eligible for an internal replay.
	})
	defer server.Close()

	result, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "before completion") {
		t.Fatalf("error=%v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("buffered partial response replayed on the same account: attempts=%d", attempts.Load())
	}
	if result.Text != "unsafe partial instruction" || result.ConversationID == "" || result.SessionID == "" {
		t.Fatalf("partial recovery state was lost: %+v", result)
	}
}

func TestChatDoesNotRetryHandshakeFailure(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := NewClient()
	client.WSBase = "ws" + strings.TrimPrefix(server.URL, "http")
	_, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if err == nil {
		t.Fatal("expected handshake failure")
	}
	if attempts.Load() != 1 {
		t.Fatalf("handshake failure replayed on the same account: attempts=%d", attempts.Load())
	}
}

func TestChatDoesNotRetryPartialOutputAfterCallerCommittedIt(t *testing.T) {
	var attempts atomic.Int32
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		attempts.Add(1)
		writeSignalR(t, conn, map[string]any{
			"type":   1,
			"target": "update",
			"arguments": []any{map[string]any{
				"writeAtCursor": "visible partial",
			}},
		})
	})
	defer server.Close()

	_, err := client.ChatWithEvents(context.Background(), testAccount(), Request{Text: "hi"}, func(StreamEvent) (bool, error) {
		return true, nil
	})
	if err == nil || !strings.Contains(err.Error(), "before completion") {
		t.Fatalf("error=%v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("committed stream replayed attempts=%d", attempts.Load())
	}
}

func TestChatHTTP429IsTypedAndNotRetried(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "17")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := NewClient()
	client.WSBase = "ws" + strings.TrimPrefix(server.URL, "http")
	_, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
	var rateLimit *RateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("error type=%T value=%v", err, err)
	}
	if rateLimit.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d", rateLimit.StatusCode)
	}
	if rateLimit.RetryAfter != 17*time.Second || rateLimit.RetryAt.IsZero() {
		t.Fatalf("retry after=%s at=%s", rateLimit.RetryAfter, rateLimit.RetryAt)
	}
	if attempts.Load() != 1 {
		t.Fatalf("429 retried on same account: attempts=%d", attempts.Load())
	}
}

func TestChatQuotaExhaustedIsTypedAndNotRetried(t *testing.T) {
	tests := []struct {
		name       string
		throttling map[string]any
	}{
		{name: "numeric CostQuota", throttling: map[string]any{"CostQuota": 0}},
		{name: "nested CostQuota", throttling: map[string]any{"CostQuota": map[string]any{"remainingAllowance": 0}}},
		{name: "metering CostQuota", throttling: map[string]any{"metering": map[string]any{"CostQuota": map[string]any{"remainingAllowance": 0}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			server, client := testChatHubServer(t, func(conn *websocket.Conn) {
				attempts.Add(1)
				writeSignalR(t, conn,
					map[string]any{"type": 2, "item": map[string]any{"throttling": tc.throttling}},
					map[string]any{"type": 3},
				)
			})
			defer server.Close()

			_, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
			var rateLimit *RateLimitError
			if !errors.As(err, &rateLimit) {
				t.Fatalf("error type=%T value=%v", err, err)
			}
			if rateLimit.StatusCode != 0 || !strings.Contains(rateLimit.Error(), "CostQuota") {
				t.Fatalf("rate limit=%+v", rateLimit)
			}
			if attempts.Load() != 1 {
				t.Fatalf("quota failure retried on same account: attempts=%d", attempts.Load())
			}
		})
	}
}

func TestChatKeepsCompletedAnswerAtQuotaBoundary(t *testing.T) {
	var attempts atomic.Int32
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		attempts.Add(1)
		writeSignalR(t, conn,
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{
				"writeAtCursor": "last successful answer",
				"throttling":    map[string]any{"CostQuota": 0},
			}}},
			map[string]any{"type": 3},
		)
	})
	defer server.Close()

	res, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "last successful answer" || attempts.Load() != 1 {
		t.Fatalf("text=%q attempts=%d", res.Text, attempts.Load())
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, time.August, 22, 0, 0, 0, 0, time.UTC)
	delay, retryAt := parseRetryAfter(now.Add(45*time.Second).Format(http.TimeFormat), now)
	if delay != 45*time.Second || !retryAt.Equal(now.Add(45*time.Second)) {
		t.Fatalf("delay=%s retryAt=%s", delay, retryAt)
	}
}

func TestSignalRHandshakeValidation(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{name: "ack", payload: "{}" + rs},
		{name: "rejected", payload: `{"error":"unauthorized"}` + rs, wantErr: true},
		{name: "unexpected frame", payload: `{"type":6}` + rs, wantErr: true},
		{name: "malformed", payload: `{` + rs, wantErr: true},
		{name: "empty", payload: rs, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSignalRHandshake([]byte(tc.payload))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%t", err, tc.wantErr)
			}
		})
	}
}

func TestChatLastUpdateBoundsMissingCompletion(t *testing.T) {
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		writeSignalR(t, conn, map[string]any{
			"type":   1,
			"target": "update",
			"arguments": []any{map[string]any{
				"writeAtCursor": "partial answer",
				"isLastUpdate":  true,
			}},
		})
		_, _, _ = conn.ReadMessage()
	})
	defer server.Close()
	client.FinalFrameTimeout = 40 * time.Millisecond

	started := time.Now()
	_, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "ws read before completion") {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("missing completion waited too long: %s", elapsed)
	}
}

func TestChatTypeTwoBoundsMissingCompletion(t *testing.T) {
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		writeSignalR(t, conn, map[string]any{
			"type": 2,
			"item": map[string]any{"result": map[string]any{"message": "final text"}},
		})
		_, _, _ = conn.ReadMessage()
	})
	defer server.Close()
	client.FinalFrameTimeout = 40 * time.Millisecond

	started := time.Now()
	_, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "ws read before completion") {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("missing completion waited too long: %s", elapsed)
	}
}

func TestChatTerminalProgressRefreshesFinalFrameGrace(t *testing.T) {
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		writeSignalR(t, conn, map[string]any{
			"type":   1,
			"target": "update",
			"arguments": []any{map[string]any{
				"writeAtCursor": "complete answer",
				"isLastUpdate":  true,
			}},
		})
		time.Sleep(100 * time.Millisecond)
		writeSignalR(t, conn, map[string]any{
			"type": 2,
			"item": map[string]any{"result": map[string]any{"message": "complete answer"}},
		})
		time.Sleep(100 * time.Millisecond)
		writeSignalR(t, conn, map[string]any{"type": 3})
	})
	defer server.Close()
	client.FinalFrameTimeout = 150 * time.Millisecond

	res, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "complete answer" {
		t.Fatalf("text=%q", res.Text)
	}
}

func TestChatBoundsRetainedProtocolEvents(t *testing.T) {
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		frames := make([]map[string]any, 0, maxRetainedEventCount+12)
		for i := 0; i < maxRetainedEventCount+10; i++ {
			frames = append(frames, map[string]any{"type": 99, "sequence": i})
		}
		frames = append(frames,
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"writeAtCursor": "ok"}}},
			map[string]any{"type": 3},
		)
		writeSignalR(t, conn, frames...)
	})
	defer server.Close()

	res, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "ok" {
		t.Fatalf("text=%q", res.Text)
	}
	if !res.EventsTruncated || len(res.Events) != maxRetainedEventCount {
		t.Fatalf("truncated=%t retained=%d", res.EventsTruncated, len(res.Events))
	}
	var last map[string]any
	if err := json.Unmarshal(res.Events[len(res.Events)-1], &last); err != nil || int(last["type"].(float64)) != 3 {
		t.Fatalf("last retained event=%s err=%v", res.Events[len(res.Events)-1], err)
	}
}

func TestChatRejectsMalformedSignalRFrame(t *testing.T) {
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":1`+rs)); err != nil {
			t.Errorf("write malformed frame: %v", err)
		}
	})
	defer server.Close()

	_, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "signalr protocol") {
		t.Fatalf("error=%v", err)
	}
}

func TestChatToleratesUnknownWellFormedFrame(t *testing.T) {
	server, client := testChatHubServer(t, func(conn *websocket.Conn) {
		writeSignalR(t, conn,
			map[string]any{"type": 99, "future": true},
			map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"writeAtCursor": "ok"}}},
			map[string]any{"type": 3},
		)
	})
	defer server.Close()

	res, err := client.Chat(context.Background(), testAccount(), Request{Text: "hi"})
	if err != nil || res.Text != "ok" {
		t.Fatalf("text=%q error=%v", res.Text, err)
	}
}
