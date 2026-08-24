package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

const (
	debugMaxRecords       = 500
	debugSnippetBytes     = 4 << 10
	debugValueBytes       = 10 << 10
	debugRecordBytes      = 24 << 10
	debugPathBytes        = 1 << 10
	debugLogMaxBytes      = int64(10 << 20)
	debugLogMaxFiles      = 3 // active file plus two rotated files
	debugWriteErrorPeriod = time.Minute
)

var (
	sensitiveJSONFieldRE = regexp.MustCompile(`(?i)("(?:api[_-]?key|authorization|proxy[_-]?authorization|access[_-]?token|refresh[_-]?token|id[_-]?token|password|client[_-]?secret|secret|cookie|proxy)"\s*:\s*)"(?:\\.|[^"\\])*"`)
	truncatedSecretRE    = regexp.MustCompile(`(?i)("(?:api[_-]?key|authorization|proxy[_-]?authorization|access[_-]?token|refresh[_-]?token|id[_-]?token|password|client[_-]?secret|secret|cookie|proxy)"\s*:\s*")[^"]*$`)
	bearerTokenRE        = regexp.MustCompile(`(?i)\bbearer[ \t]+[a-z0-9._~+/=-]+`)
	proxyCredentialRE    = regexp.MustCompile(`(?i)\b(https?|socks5h?|socks4)://[^/@\s:]+:[^/@\s]+@`)
)

type debugRecord struct {
	ID           string    `json:"id"`
	RequestID    string    `json:"requestId,omitempty"`
	At           time.Time `json:"at"`
	Path         string    `json:"path"`
	Method       string    `json:"method"`
	Status       int       `json:"status"`
	Level        string    `json:"level"`
	DurationMS   int64     `json:"durationMs"`
	InputTokens  *int      `json:"inputTokens"`
	OutputTokens *int      `json:"outputTokens"`
	TokenSource  string    `json:"tokenSource"`
	CacheHit     *bool     `json:"cacheHit"`
	CacheSource  string    `json:"cacheSource"`
	Client       any       `json:"client"`
	Upstream     any       `json:"upstream"`
	Gateway      any       `json:"gateway"`
}

type debugStore struct {
	mu             sync.RWMutex
	records        []debugRecord
	path           string
	maxFileBytes   int64
	maxFiles       int
	lastWriteError time.Time
}

func openDebugStore() *debugStore {
	applyDefaultDebugLogLevel()
	p := strings.TrimSpace(os.Getenv("M365_DEBUG_LOG"))
	if p == "" {
		p = "debug-logs.jsonl"
	}
	return &debugStore{path: p, maxFileBytes: debugLogMaxBytes, maxFiles: debugLogMaxFiles}
}

// applyDefaultDebugLogLevel changes only the implicit default. An environment
// value or a persisted setting remains authoritative, and a later admin update
// can still switch the level dynamically.
func applyDefaultDebugLogLevel() {
	if strings.TrimSpace(os.Getenv("M365_LOG_LEVEL")) != "" {
		return
	}
	if b, err := os.ReadFile(settingsPath()); err == nil {
		var raw map[string]json.RawMessage
		if json.Unmarshal(b, &raw) == nil {
			var level string
			if value, ok := raw["logLevel"]; ok && json.Unmarshal(value, &level) == nil && strings.TrimSpace(level) != "" {
				return
			}
		}
	}
	settings, err := openSettingsStore()
	if err != nil {
		return
	}
	settings.mu.Lock()
	if settings.v.LogLevel == "" || settings.v.LogLevel == "info" {
		settings.v.LogLevel = "warn"
	}
	settings.mu.Unlock()
}

func sensitiveDebugKey(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(strings.TrimSpace(key)))
	switch normalized {
	case "apikey", "authorization", "proxyauthorization", "accesstoken", "refreshtoken", "idtoken", "token", "password", "clientsecret", "secret", "cookie", "setcookie", "proxy":
		return true
	default:
		return strings.HasSuffix(normalized, "password") || strings.HasSuffix(normalized, "secret")
	}
}

func redactDebugValue(key string, value any) any {
	if sensitiveDebugKey(key) {
		return "[redacted]"
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for childKey, childValue := range v {
			out[childKey] = redactDebugValue(childKey, childValue)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = redactDebugValue("", v[i])
		}
		return out
	default:
		return value
	}
}

func redactRawSnippet(raw string) string {
	raw = sensitiveJSONFieldRE.ReplaceAllString(raw, `${1}"[redacted]"`)
	raw = truncatedSecretRE.ReplaceAllString(raw, `${1}[redacted]`)
	raw = bearerTokenRE.ReplaceAllString(raw, "Bearer [redacted]")
	return proxyCredentialRE.ReplaceAllString(raw, `${1}://[redacted]@`)
}

func redactBody(b []byte) any {
	var value any
	if json.Unmarshal(b, &value) != nil {
		return redactRawSnippet(string(b))
	}
	return redactDebugValue("", value)
}

func debugLevel(status int) string {
	if status >= 500 {
		return "error"
	}
	if status >= 400 {
		return "warn"
	}
	return "info"
}

func debugLevelRank(level string) int {
	switch level {
	case "debug":
		return 0
	case "info":
		return 1
	case "warn":
		return 2
	case "error":
		return 3
	case "silent":
		return 4
	}
	return 1
}

func configuredDebugLevel() string {
	if level := strings.TrimSpace(os.Getenv("M365_LOG_LEVEL")); level != "" {
		return level
	}
	level := strings.TrimSpace(currentSettings().LogLevel)
	if level == "" {
		return "warn"
	}
	return level
}

func truncateDebugString(value string, maxBytes int) string {
	if maxBytes < 1 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "…"
}

func boundDebugValue(value any, maxBytes int) any {
	b, err := json.Marshal(value)
	if err != nil {
		return map[string]any{"captured": false, "reason": "unserializable"}
	}
	if len(b) <= maxBytes {
		return value
	}
	return map[string]any{"captured": false, "reason": "value_too_large", "bytes": len(b)}
}

func boundDebugRecord(record debugRecord) debugRecord {
	record.ID = truncateDebugString(record.ID, 256)
	record.RequestID = truncateDebugString(record.RequestID, 256)
	record.Path = truncateDebugString(record.Path, debugPathBytes)
	record.Method = truncateDebugString(record.Method, 32)
	record.Level = truncateDebugString(record.Level, 16)
	record.TokenSource = truncateDebugString(record.TokenSource, 128)
	record.CacheSource = truncateDebugString(record.CacheSource, 128)
	record.Client = boundDebugValue(record.Client, debugValueBytes)
	record.Upstream = boundDebugValue(record.Upstream, debugValueBytes)
	record.Gateway = boundDebugValue(record.Gateway, debugValueBytes)

	if b, err := json.Marshal(record); err != nil || len(b) > debugRecordBytes {
		record.Client = map[string]any{"captured": false, "reason": "record_size_limit"}
		record.Upstream = map[string]any{"captured": false, "reason": "record_size_limit"}
		record.Gateway = map[string]any{"captured": false, "reason": "record_size_limit"}
	}
	return record
}

func (d *debugStore) fileLimits() (int64, int) {
	maxBytes, maxFiles := d.maxFileBytes, d.maxFiles
	if maxBytes <= 0 {
		maxBytes = debugLogMaxBytes
	}
	if maxFiles <= 0 {
		maxFiles = debugLogMaxFiles
	}
	return maxBytes, maxFiles
}

func (d *debugStore) rotateLocked(maxFiles int) error {
	if maxFiles <= 1 {
		if err := os.Remove(d.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	oldest := fmt.Sprintf("%s.%d", d.path, maxFiles-1)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		return err
	}
	for index := maxFiles - 2; index >= 1; index-- {
		source := fmt.Sprintf("%s.%d", d.path, index)
		destination := fmt.Sprintf("%s.%d", d.path, index+1)
		if err := os.Rename(source, destination); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(d.path, d.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (d *debugStore) appendLocked(line []byte) error {
	maxBytes, maxFiles := d.fileLimits()
	if int64(len(line)) > maxBytes {
		return fmt.Errorf("debug record is %d bytes, larger than log limit %d", len(line), maxBytes)
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return err
	}
	if stat, err := os.Stat(d.path); err == nil && stat.Size()+int64(len(line)) > maxBytes {
		if err := d.rotateLocked(maxFiles); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(d.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	_, writeErr := f.Write(line)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func (d *debugStore) reportWriteErrorLocked(err error) {
	if time.Since(d.lastWriteError) < debugWriteErrorPeriod {
		return
	}
	d.lastWriteError = time.Now()
	log.Printf("[debug-log] persistence failed: %v", err)
}

func (d *debugStore) add(record debugRecord) {
	configured := configuredDebugLevel()
	if configured == "silent" || debugLevelRank(record.Level) < debugLevelRank(configured) {
		return
	}
	record = boundDebugRecord(record)
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	line = append(line, '\n')

	d.mu.Lock()
	defer d.mu.Unlock()
	d.records = append(d.records, record)
	if len(d.records) > debugMaxRecords {
		d.records = append([]debugRecord(nil), d.records[len(d.records)-debugMaxRecords:]...)
	}
	if err := d.appendLocked(line); err != nil {
		d.reportWriteErrorLocked(err)
	}
}

func (d *debugStore) list() []debugRecord {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := append([]debugRecord(nil), d.records...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (d *debugStore) get(id string) (debugRecord, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, record := range d.records {
		if record.ID == id {
			return record, true
		}
	}
	return debugRecord{}, false
}

type byteCapture struct {
	total int64
	body  bytes.Buffer
	tail  bytes.Buffer
}

func (c *byteCapture) write(p []byte) {
	c.total += int64(len(p))
	_, _ = c.tail.Write(p)
	if c.tail.Len() > debugSnippetBytes {
		kept := append([]byte(nil), c.tail.Bytes()[c.tail.Len()-debugSnippetBytes:]...)
		c.tail.Reset()
		_, _ = c.tail.Write(kept)
	}
	remaining := debugSnippetBytes - c.body.Len()
	if remaining <= 0 {
		return
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	_, _ = c.body.Write(p)
}

func (c *byteCapture) view(includeBody bool, contentLength int64) any {
	view := map[string]any{
		"bytes":     c.total,
		"truncated": c.total > int64(c.body.Len()),
	}
	if contentLength >= 0 {
		view["contentLength"] = contentLength
	}
	if includeBody && c.body.Len() > 0 {
		view["body"] = boundDebugValue(redactBody(c.body.Bytes()), debugValueBytes)
		if c.total > int64(c.body.Len()) && c.tail.Len() > 0 {
			view["tail"] = boundDebugValue(redactBody(c.tail.Bytes()), debugValueBytes)
		}
	}
	return view
}

func (c *byteCapture) hasStreamTerminalFailure() bool {
	if c == nil {
		return false
	}
	snippet := string(c.body.Bytes())
	if c.total > int64(c.body.Len()) {
		snippet += "\n" + string(c.tail.Bytes())
	}
	for _, marker := range []string{
		`"type":"response.failed"`,
		`"type":"upstream_error"`,
		`"type":"session_persistence_error"`,
		`"code":"required_tool_missing"`,
		`"code":"tool_loop_detected"`,
		"event: error",
	} {
		if strings.Contains(snippet, marker) {
			return true
		}
	}
	return false
}

type captureReadCloser struct {
	io.ReadCloser
	capture byteCapture
}

func (c *captureReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 {
		c.capture.write(p[:n])
	}
	return n, err
}

type captureWriter struct {
	http.ResponseWriter
	status  int
	capture byteCapture
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *captureWriter) WriteHeader(status int) {
	if c.status != 0 {
		return
	}
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *captureWriter) Flush() {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if flusher, ok := c.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (c *captureWriter) Header() http.Header { return c.ResponseWriter.Header() }

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(p)
	if n > 0 {
		c.capture.write(p[:n])
	}
	return n, err
}

func (s *Server) debugMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}

		requestBody := r.Body
		if requestBody == nil {
			requestBody = http.NoBody
		}
		requestCapture := &captureReadCloser{ReadCloser: requestBody}
		r.Body = requestCapture
		responseCapture := &captureWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(responseCapture, r)
		if responseCapture.status == 0 {
			responseCapture.status = http.StatusOK
		}
		streamTerminalFailure := responseCapture.capture.hasStreamTerminalFailure()
		includeBody := responseCapture.status >= http.StatusBadRequest || streamTerminalFailure
		level := debugLevel(responseCapture.status)
		if streamTerminalFailure {
			level = "error"
		}
		record := debugRecord{
			ID:          "dbg_" + uuid.NewString(),
			RequestID:   chathub.CorrelationID(r.Context()),
			At:          start,
			Level:       level,
			Path:        r.URL.Path,
			Method:      r.Method,
			Status:      responseCapture.status,
			DurationMS:  time.Since(start).Milliseconds(),
			TokenSource: "unavailable_from_chathub",
			CacheSource: "not_reported_by_upstream",
			Client:      requestCapture.capture.view(includeBody, r.ContentLength),
			Gateway:     responseCapture.capture.view(includeBody, -1),
			Upstream: map[string]any{
				"captured": false,
				"reason":   "ChatHub transport tracing not attached to request context",
			},
		}
		s.debug.add(record)
	})
}

func (s *Server) debugList(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, map[string]any{"records": s.debug.list()})
}

func (s *Server) debugDetail(w http.ResponseWriter, r *http.Request) {
	if record, ok := s.debug.get(r.URL.Query().Get("id")); ok {
		jsonOut(w, record)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}
