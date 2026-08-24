package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSSEKeepalivePreventsIdleStream(t *testing.T) {
	oldTick, oldIdle, oldTimeout := sseHeartbeatTick, sseHeartbeatIdle, sseHeartbeatWriteTimeout
	sseHeartbeatTick = 5 * time.Millisecond
	sseHeartbeatIdle = 15 * time.Millisecond
	sseHeartbeatWriteTimeout = 100 * time.Millisecond
	defer func() {
		sseHeartbeatTick, sseHeartbeatIdle, sseHeartbeatWriteTimeout = oldTick, oldIdle, oldTimeout
	}()
	handler := sseKeepaliveMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(40 * time.Millisecond)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	server := httptest.NewServer(handler)
	defer server.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, ": keepalive\n\n") {
		t.Fatalf("missing SSE keepalive: %q", text)
	}
	if !strings.Contains(text, "data: [DONE]\n\n") {
		t.Fatalf("missing final SSE frame: %q", text)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering=%q", got)
	}
}

func TestSSEKeepaliveIsIdleAware(t *testing.T) {
	oldTick, oldIdle, oldTimeout := sseHeartbeatTick, sseHeartbeatIdle, sseHeartbeatWriteTimeout
	sseHeartbeatTick = 2 * time.Millisecond
	sseHeartbeatIdle = 100 * time.Millisecond
	sseHeartbeatWriteTimeout = 100 * time.Millisecond
	defer func() {
		sseHeartbeatTick, sseHeartbeatIdle, sseHeartbeatWriteTimeout = oldTick, oldIdle, oldTimeout
	}()
	handler := sseKeepaliveMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 5; i++ {
			_, _ = io.WriteString(w, "data: tick\n\n")
			time.Sleep(5 * time.Millisecond)
		}
	}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rr.Body.String(), ": keepalive\n\n") {
		t.Fatalf("heartbeat emitted while data was active: %q", rr.Body.String())
	}
}
