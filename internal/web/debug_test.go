package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDebugDefaultsToWarn(t *testing.T) {
	oldSettings := sharedSettings
	t.Cleanup(func() { sharedSettings = oldSettings })
	t.Setenv("M365_LOG_LEVEL", "")
	t.Setenv("M365_SETTINGS_FILE", filepath.Join(t.TempDir(), "settings.json"))
	t.Setenv("M365_DEBUG_LOG", filepath.Join(t.TempDir(), "debug.jsonl"))
	sharedSettings = &settingsStore{path: settingsPath(), v: defaultRuntimeSettings()}
	store := openDebugStore()
	if got := sharedSettings.get().LogLevel; got != "warn" {
		t.Fatalf("default log level=%q", got)
	}
	store.add(debugRecord{ID: "ok", Level: "info", Status: http.StatusOK})
	if len(store.records) != 0 {
		t.Fatalf("default warn level retained %d info records", len(store.records))
	}
}

func TestDebugMiddlewareDoesNotPreReadLargeRequest(t *testing.T) {
	t.Setenv("M365_LOG_LEVEL", "warn")
	store := &debugStore{path: filepath.Join(t.TempDir(), "debug.jsonl")}
	server := &Server{debug: store}
	body := strings.Repeat("x", 2<<20)
	handler := server.debugMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 16)
		_, _ = r.Body.Read(buf)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if len(store.records) != 1 {
		t.Fatalf("records=%d", len(store.records))
	}
	client, ok := store.records[0].Client.(map[string]any)
	if !ok {
		t.Fatalf("client=%#v", store.records[0].Client)
	}
	if got := client["bytes"]; got != int64(16) {
		t.Fatalf("middleware pre-read request: bytes=%v", got)
	}
	if got := client["contentLength"]; got != int64(len(body)) {
		t.Fatalf("contentLength=%v", got)
	}
}

func TestDebugMiddlewareBoundsLargeRequestAndSSEResponse(t *testing.T) {
	t.Setenv("M365_LOG_LEVEL", "warn")
	store := &debugStore{path: filepath.Join(t.TempDir(), "debug.jsonl")}
	server := &Server{debug: store}
	requestBody := `{"outer":{"access_token":"request-secret","message":"` + strings.Repeat("r", 2<<20) + `"}}`
	responsePrefix := "data: {\"refresh_token\":\"response-secret\"}\n\n"
	responseChunk := responsePrefix + strings.Repeat("s", 32<<10)
	handler := server.debugMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusInternalServerError)
		for i := 0; i < 64; i++ {
			_, _ = io.WriteString(w, responseChunk)
		}
	}))

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if len(store.records) != 1 {
		t.Fatalf("records=%d", len(store.records))
	}
	record := store.records[0]
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > debugRecordBytes {
		t.Fatalf("record size=%d limit=%d", len(encoded), debugRecordBytes)
	}
	if strings.Contains(string(encoded), "request-secret") || strings.Contains(string(encoded), "response-secret") {
		t.Fatalf("secret leaked: %s", encoded)
	}
	client := record.Client.(map[string]any)
	if got := client["bytes"]; got != int64(len(requestBody)) {
		t.Fatalf("request bytes=%v want=%d", got, len(requestBody))
	}
	if got := client["truncated"]; got != true {
		t.Fatalf("request truncated=%v", got)
	}
	gateway := record.Gateway.(map[string]any)
	wantResponseBytes := int64(len(responseChunk) * 64)
	if got := gateway["bytes"]; got != wantResponseBytes {
		t.Fatalf("response bytes=%v want=%d", got, wantResponseBytes)
	}
	if got := gateway["truncated"]; got != true {
		t.Fatalf("response truncated=%v", got)
	}
	if store.records[0].Status != http.StatusInternalServerError {
		t.Fatalf("status=%d", store.records[0].Status)
	}
}

func TestDebugMiddlewareElevatesTerminalSSEFailureAfterHTTP200(t *testing.T) {
	t.Setenv("M365_LOG_LEVEL", "warn")
	store := &debugStore{path: filepath.Join(t.TempDir(), "debug.jsonl")}
	server := &Server{debug: store}
	handler := requestID(server.debugMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+strings.Repeat("x", debugSnippetBytes*2)+"\n\n")
		_, _ = io.WriteString(w, `data: {"error":{"message":"ws read timeout","type":"upstream_error","code":"upstream_error"}}`+"\n\n")
	})))

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("wire status=%d want 200 after SSE started", response.Code)
	}
	if len(store.records) != 1 {
		t.Fatalf("terminal SSE failure was omitted at warn level: records=%d", len(store.records))
	}
	record := store.records[0]
	if record.Level != "error" || record.RequestID == "" {
		t.Fatalf("record level=%q requestId=%q", record.Level, record.RequestID)
	}
	gateway, ok := record.Gateway.(map[string]any)
	if !ok || gateway["tail"] == nil {
		t.Fatalf("terminal tail was not retained: %#v", record.Gateway)
	}
}

func TestDebugRecursiveAndPartialRedaction(t *testing.T) {
	value := redactBody([]byte(`{"outer":[{"access_token":"one","nested":{"password":"two"}},{"safe":"visible"}],"proxy":"http://user:pass@example:8080"}`))
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{"one", "two", "user:pass"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%q leaked in %s", secret, text)
		}
	}
	if !strings.Contains(text, "visible") || strings.Count(text, "[redacted]") < 3 {
		t.Fatalf("unexpected redaction: %s", text)
	}

	partial := redactRawSnippet(`{"nested":{"refresh_token":"partial-secret`)
	if strings.Contains(partial, "partial-secret") || !strings.Contains(partial, "[redacted]") {
		t.Fatalf("partial secret was not redacted: %s", partial)
	}
	bearer := redactRawSnippet("Authorization: Bearer abc.def.ghi")
	if strings.Contains(bearer, "abc.def.ghi") {
		t.Fatalf("bearer token leaked: %s", bearer)
	}
}

func TestDebugLogRotationKeepsAtMostThreeFiles(t *testing.T) {
	t.Setenv("M365_LOG_LEVEL", "error")
	path := filepath.Join(t.TempDir(), "debug.jsonl")
	store := &debugStore{path: path, maxFileBytes: 2 << 10, maxFiles: 3}
	for i := 0; i < 30; i++ {
		store.add(debugRecord{
			ID:      "record-" + strings.Repeat("x", 16),
			At:      time.Unix(int64(i), 0).UTC(),
			Path:    "/v1/responses",
			Method:  http.MethodPost,
			Status:  http.StatusInternalServerError,
			Level:   "error",
			Client:  map[string]any{"body": strings.Repeat("c", 700)},
			Gateway: map[string]any{"body": strings.Repeat("g", 700)},
		})
	}

	for _, candidate := range []string{path, path + ".1", path + ".2"} {
		stat, err := os.Stat(candidate)
		if err != nil {
			t.Fatalf("missing rotated file %s: %v", candidate, err)
		}
		if stat.Size() > store.maxFileBytes {
			t.Fatalf("%s size=%d limit=%d", candidate, stat.Size(), store.maxFileBytes)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("unexpected fourth log file: %v", err)
	}
}

func TestCaptureWriterUnwrapAndBoundedCapture(t *testing.T) {
	underlying := httptest.NewRecorder()
	writer := &captureWriter{ResponseWriter: underlying}
	if writer.Unwrap() != underlying {
		t.Fatal("Unwrap did not return the underlying ResponseWriter")
	}
	payload := []byte(strings.Repeat("z", 1<<20))
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if writer.capture.total != int64(len(payload)) {
		t.Fatalf("bytes=%d want=%d", writer.capture.total, len(payload))
	}
	if writer.capture.body.Len() != debugSnippetBytes {
		t.Fatalf("captured=%d limit=%d", writer.capture.body.Len(), debugSnippetBytes)
	}
}
