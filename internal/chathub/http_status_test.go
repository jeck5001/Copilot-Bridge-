package chathub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSocketUpgradePreservesHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer server.Close()
	client := NewClient()
	client.WSBase = "ws" + strings.TrimPrefix(server.URL, "http")
	_, err := client.Chat(context.Background(), testAccount(), Request{Text: "hello"})
	var statusError *HTTPStatusError
	if !errors.As(err, &statusError) {
		t.Fatalf("error type=%T value=%v; want HTTPStatusError", err, err)
	}
	if statusError.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d; want 401", statusError.StatusCode)
	}
}
