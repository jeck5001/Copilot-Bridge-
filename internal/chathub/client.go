package chathub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/vipamess/Copilot-Bridge-/internal/proxy"
)

// ErrDisengaged is the upstream safety-filter refusal (messageType
// "Disengaged": empty content, offense usually "None"). Retrying the same
// prompt re-disengages and burns conversation quota, so it must be returned to
// the account-selection layer immediately. Callers match with errors.Is.
var ErrDisengaged = errors.New("chathub disengaged: upstream safety filter refused this prompt; do not retry unchanged")

// RateLimitError is an explicit upstream rate-limit signal. Callers should use
// errors.As instead of matching error text. RetryAt is populated when the
// upstream supplies a valid Retry-After header; RetryAfter is the corresponding
// non-negative delay calculated when the response was received.
type RateLimitError struct {
	StatusCode int
	RetryAfter time.Duration
	RetryAt    time.Time
	Reason     string
}

// HTTPStatusError preserves the status returned by the WebSocket upgrade.
// gorilla/websocket otherwise collapses responses such as 401, 403 and 5xx
// into the indistinguishable text "bad handshake", which prevents the account
// router from applying the correct health policy.
type HTTPStatusError struct {
	StatusCode int
	Reason     string
	Err        error
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return "chathub HTTP error"
	}
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		reason = http.StatusText(e.StatusCode)
	}
	if reason == "" {
		reason = "upstream WebSocket upgrade failed"
	}
	return fmt.Sprintf("chathub WebSocket HTTP %d: %s", e.StatusCode, reason)
}

func (e *HTTPStatusError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *RateLimitError) Error() string {
	if e == nil {
		return "chathub rate limited"
	}
	message := strings.TrimSpace(e.Reason)
	if message == "" {
		message = "upstream rate limit"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("chathub rate limited (HTTP %d): %s", e.StatusCode, message)
	}
	return "chathub rate limited: " + message
}

func parseRetryAfter(raw string, now time.Time) (time.Duration, time.Time) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, time.Time{}
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
		delay := time.Duration(seconds) * time.Second
		return delay, now.Add(delay)
	}
	if retryAt, err := http.ParseTime(raw); err == nil {
		delay := retryAt.Sub(now)
		if delay < 0 {
			delay = 0
		}
		return delay, retryAt
	}
	return 0, time.Time{}
}

func quotaNumberExhausted(value any) bool {
	switch n := value.(type) {
	case float64:
		return n <= 0
	case float32:
		return n <= 0
	case int:
		return n <= 0
	case int64:
		return n <= 0
	case json.Number:
		parsed, err := n.Float64()
		return err == nil && parsed <= 0
	default:
		return false
	}
}

func quotaValueExhausted(value any) bool {
	if quotaNumberExhausted(value) {
		return true
	}
	quota, ok := value.(map[string]any)
	return ok && quotaNumberExhausted(quota["remainingAllowance"])
}

// quotaExhausted recognizes the structured CostQuota shapes observed on both
// update and completion frames. Merely having a throttling object is not a
// limit signal; the allowance must explicitly be zero or negative.
func quotaExhausted(throttling any) bool {
	root, ok := throttling.(map[string]any)
	if !ok {
		return false
	}
	if quotaValueExhausted(root["CostQuota"]) {
		return true
	}
	metering, ok := root["metering"].(map[string]any)
	return ok && quotaValueExhausted(metering["CostQuota"])
}

func quotaRateLimitError() error {
	return &RateLimitError{Reason: "Microsoft Copilot CostQuota exhausted"}
}

const (
	rs          = "\x1e"
	defaultTone = "magic"
	wsBase      = "wss://substrate.office.com/m365Copilot/Chathub"
)

// Variants mirrored from the verified browser / Python probe, extended to the
// full superset captured from live m365.cloud.microsoft sessions (captured browser-protocol
// reference). The added flags control delta merging, empty-message flushing,
// rich-answer rendering and conversation sharing — behaviour the previous
// shorter list left on defaults for.
const variants = "EnableMcpServerWidgets,feature.EnableMcpServerWidgets,feature.EnableLuForChatCIQ,feature.enableChatCIQPlugin,EnableRequestPlugins,feature.EnableSensitivityLabels,EnableUnsupportedUrlDetector,feature.IsCustomEngineCopilotEnabled,feature.bizchatfluxv3,feature.enablechatpages,feature.enableCodeCanvas,feature.turnOnWorkTabRecommendation,turnOffWorkTabUpsellFromClient,feature.turnOnDARecommendation,feature.IsStreamingModeInChatRequestEnabled,IncludeSourceAttributionsConcise,SkipPublishEmptyMessage,feature.EnableDeduplicatingSourceAttributions,Enable3PActionProgressMessages,feature.enableClientWebRtc,feature.EnableMeetingRecapOfSeriesMeetingWithCiq,feature.EnableReferencesListCompleteSignal,feature.StorageMessageSplitDisabled,feature.EnableCuaTakeControlApi,SingletonEnvOn,feature.cwcallowedos,feature.EnableMergingPureDeltas,feature.disabledisallowedmsgs,feature.enableCitationsForSynthesisData,feature.EnableConversationShareApis,feature.enableGenerateGraphicArtOptionsSet,cdximagen,feature.EnableUpdatedUXForConfirmationDialog,feature.EnableContentApiandDocTypeHtmlInRichAnswers,cdxgrounding_api_v2_rich_web_answers_reference_bottom_force,cdxenablerenderforisocomp,feature.EnableClientFileURLSupportForOfficeWebPaidCopilot,feature.EnableDesignEditorImageGrounding,feature.EnableDesignerEditor,feature.EnableSkipRehydrationForSpeCIdImages,feature.EnableSkipEmittingMessageOnFlush,feature.EnableRemoveEmptySourceAttributions,feature.EnableRemoveStreamingMode,feature.OfficeWebToHelix,feature.OfficeDesktopToHelix,feature.M365TeamsHubToHelix,feature.OwaHubToHelix,feature.MonarchHubToHelix,feature.Win32OutlookHubToHelix,feature.MacOutlookHubToHelix,Agt_bizchat_enableGpt5ForHelix"

var (
	// ChatHub agent mode narrates every tool step with the same boilerplate
	// ("我将执行：... 目的：... 预期：..."). Strip it so clients only see
	// actual content.
	narrationRe1 = regexp.MustCompile(`(?s)我将执行[：:].*?
\s*目的[：:][^
]*
?\s*预期[：:][^
]*`)
	narrationRe2 = regexp.MustCompile(`我将执行[：:][^
。]{0,120}。`)

	// Citation markers Copilot embeds in responses ([^1^], mojibake-wrapped
	// ÑciteÖ...Ô, HTML cite tags). Cleaned from deltas before delivery.
	citationPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\[\^[^\]]*\^\]`),
		// The mojibake-wrapped cite marker uses the literal bytes 0xC3 0x91
		// ("Ñ") and 0xC3 0x94 ("Ô") around "cite..."; match by code point.
		regexp.MustCompile(`\xc3\x91cite[^\xc3\x94]*\xc3\x94`),
		regexp.MustCompile(`<cite[^>]*>[^<]*</cite>`),
	}
	// codeInterpreterArtifactMarkers identify JSON metadata blobs that leak
	// into message text when the server-side Python sandbox runs. They are
	// execution bookkeeping (file stores, result URLs), never user content.
	codeInterpreterArtifactMarkers = []string{
		`"fileStoreType"`, `"codeResultFileUrl"`, `"reference_id"`,
	}
)

func scrubNarration(s string) string {
	s = narrationRe1.ReplaceAllString(s, "")
	s = narrationRe2.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// scrubCitations removes internal citation markers ([^1^], ÑciteÖ...Ô, HTML
// cite tags) that Copilot embeds in responses. Codex would otherwise render
// them as garbage references.
func scrubCitations(s string) string {
	for _, re := range citationPatterns {
		s = re.ReplaceAllString(s, "")
	}
	return s
}

const defaultFinalFrameTimeout = 60 * time.Second

func compactStreamError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", " "), "\n", " "))
	if len(s) > 320 {
		s = s[:320] + "..."
	}
	return s
}

func traceProtocolText(enabled bool, requestID, field, value string) {
	if !enabled {
		return
	}
	const limit = 512
	preview := value
	if len(preview) > limit {
		preview = preview[:limit] + "..."
	}
	log.Printf("[chathub-trace] request=%s field=%s bytes=%d text=%q", requestID, field, len(value), preview)
}

// isCodeInterpreterArtifact reports whether a message text is purely code
// interpreter execution metadata (file stores, result URLs) leaked into the
// text field. These are bookkeeping, never user content — enabled together
// with the cwc_code_interpreter optionsSets.
func isCodeInterpreterArtifact(text string) bool {
	stripped := strings.TrimSpace(text)
	if !strings.HasPrefix(stripped, "{") && !strings.HasPrefix(stripped, "[") {
		return false
	}
	hits := 0
	for _, marker := range codeInterpreterArtifactMarkers {
		if strings.Contains(stripped, marker) {
			hits++
			if hits >= 2 {
				return true
			}
		}
	}
	return false
}

type Account struct {
	AccessToken string
	OID         string
	TID         string
	Proxy       string
}

type Request struct {
	Text           string
	Tone           string
	ConversationID string
	SessionID      string
	Attachments    []Attachment
	Tools          []Tool
	ToolChoice     any
	// Started is true only for the first turn of a ChatHub conversation.
	Started bool
}

// StreamEvent is the protocol-neutral event exposed while ChatHub is still
// producing a response. Text events are safe to show immediately; progress and
// tool events are normally buffered by protocol adapters.
type StreamEvent struct {
	Kind        string
	Text        string
	MessageType string
	ContentType string
	ToolName    string
	Arguments   json.RawMessage
	Raw         json.RawMessage
}

// StreamHandler reports whether the event was committed to the downstream
// client. Buffered progress/tool events return committed=false so a WebSocket
// drop may still be retried safely without duplicating visible output.
type StreamHandler func(StreamEvent) (committed bool, err error)

type deltaHandler func(string) (committed bool, err error)

type Result struct {
	FullText        string
	Text            string
	ConversationID  string
	SessionID       string
	RequestID       string
	Throttling      any
	RawResult       string
	Events          []json.RawMessage
	EventsTruncated bool
	Normalized      []Event
	Images          []string
}

type Client struct {
	HTTPHeader        http.Header
	Dialer            *websocket.Dialer
	WSBase            string
	PingInterval      time.Duration
	FinalFrameTimeout time.Duration
}

const (
	maxSignalRMessageBytes = 16 << 20
	maxRetainedEventCount  = 2048
	maxRetainedEventBytes  = 8 << 20
)

func NewClient() *Client {
	h := make(http.Header)
	h.Set("Origin", "https://m365.cloud.microsoft")
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0")
	return &Client{
		HTTPHeader: h,
		Dialer: &websocket.Dialer{
			HandshakeTimeout: 20 * time.Second,
			// substrate frames can be large, but 256KB is ample and keeps
			// memory bounded per connection
			ReadBufferSize:  256 * 1024,
			WriteBufferSize: 16 * 1024,
			// Share TLS sessions across dials so subsequent connections to
			// the same host skip the full handshake. This only caches the
			// TLS session ticket, never the live connection itself, so it
			// is safe with ChatHub's single-use connection constraint.
			TLSClientConfig: &tls.Config{
				ClientSessionCache: tls.NewLRUClientSessionCache(32),
			},
		},
	}
}

func (c *Client) Chat(ctx context.Context, acc Account, req Request) (Result, error) {
	return c.ChatWithDelta(ctx, acc, req, nil)
}

// ChatWithEvents is the compatibility entry point for the full event stream.
// The initial implementation exposes every upstream text delta immediately;
// the existing ChatWithDelta path remains the source of truth until the
// SignalR frame parser is migrated to emit progress/tool events as well.
func (c *Client) ChatWithEvents(ctx context.Context, acc Account, req Request, handler StreamHandler) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, func(text string) (bool, error) {
		if handler == nil {
			return false, nil
		}
		return handler(StreamEvent{Kind: "text", Text: text})
	}, handler)
}

// ChatWithDelta preserves Chat semantics while exposing upstream text deltas as
// soon as SignalR delivers them. onDelta must return quickly; returning an error
// cancels the request. Full snapshot messages are retained for final-result
// reconstruction but are not emitted as deltas, preventing duplicate text.
func (c *Client) ChatWithDelta(ctx context.Context, acc Account, req Request, onDelta func(string) error) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, func(text string) (bool, error) {
		if onDelta == nil {
			return false, nil
		}
		if err := onDelta(text); err != nil {
			return false, err
		}
		return true, nil
	}, nil)
}

func (c *Client) chatWithHandlers(ctx context.Context, acc Account, req Request, onDelta deltaHandler, onEvent StreamHandler) (Result, error) {
	if acc.AccessToken == "" || acc.OID == "" || acc.TID == "" {
		return Result{}, fmt.Errorf("missing access token / oid / tid")
	}
	if strings.TrimSpace(req.Text) == "" {
		return Result{}, fmt.Errorf("empty prompt")
	}
	if req.Tone == "" {
		req.Tone = defaultTone
	}
	firstTurn := req.Started
	if req.SessionID == "" {
		req.SessionID = uuid.NewString()
		firstTurn = true
	}
	if req.ConversationID == "" {
		req.ConversationID = uuid.NewString()
		firstTurn = true
	}

	// A Chat call owns exactly one upstream attempt. Even when no response has
	// reached the downstream caller, the payload may already have been accepted
	// by Microsoft; replaying it here can duplicate a turn or a tool call. Return
	// every dial, handshake, send, read, quota and completion error immediately so
	// the web account-selection layer can advance to the next account in order.
	emitted := false
	return c.runChat(ctx, acc, req, firstTurn, onDelta, onEvent, &emitted)
}

func (c *Client) runChat(ctx context.Context, acc Account, req Request, firstTurn bool, onDelta deltaHandler, onEvent StreamHandler, emitted *bool) (result Result, runErr error) {
	requestID := uuid.NewString()
	correlationID := CorrelationID(ctx)
	startedAt := time.Now()
	totalEventCount := 0
	totalEventBytes := 0
	updateFrameCount := 0
	resultFrameCount := 0
	completionFrameCount := 0
	unknownFrameCount := 0
	defer func() {
		shortRequestID := requestID
		if len(shortRequestID) > 8 {
			shortRequestID = shortRequestID[:8]
		}
		shortCorrelationID := correlationID
		if len(shortCorrelationID) > 12 {
			shortCorrelationID = shortCorrelationID[:12]
		}
		if runErr != nil {
			log.Printf("[chathub] correlation=%s request=%s outcome=error stage=%s reason=%q duration_ms=%d events=%d event_bytes=%d updates=%d results=%d completions=%d unknown=%d", shortCorrelationID, shortRequestID, chatHubErrorStage(runErr), compactStreamError(runErr), time.Since(startedAt).Milliseconds(), totalEventCount, totalEventBytes, updateFrameCount, resultFrameCount, completionFrameCount, unknownFrameCount)
			return
		}
		log.Printf("[chathub] correlation=%s request=%s outcome=success duration_ms=%d events=%d event_bytes=%d updates=%d results=%d completions=%d unknown=%d retained=%d truncated=%t", shortCorrelationID, shortRequestID, time.Since(startedAt).Milliseconds(), totalEventCount, totalEventBytes, updateFrameCount, resultFrameCount, completionFrameCount, unknownFrameCount, len(result.Events), result.EventsTruncated)
	}()

	wsURL, err := buildWSURLForBase(c.WSBase, acc, req.SessionID, req.ConversationID, requestID)
	if err != nil {
		return Result{}, err
	}

	dialer, err := proxy.WebSocketDialerFor(c.Dialer, acc.Proxy)
	if err != nil {
		return Result{}, fmt.Errorf("proxy dialer: %w", err)
	}
	conn, response, err := dialer.DialContext(ctx, wsURL, c.HTTPHeader.Clone())
	if err != nil {
		if response != nil {
			if response.Body != nil {
				defer response.Body.Close()
			}
			if response.StatusCode == http.StatusTooManyRequests {
				delay, retryAt := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
				return Result{}, &RateLimitError{
					StatusCode: response.StatusCode,
					RetryAfter: delay,
					RetryAt:    retryAt,
					Reason:     http.StatusText(response.StatusCode),
				}
			}
			return Result{}, &HTTPStatusError{
				StatusCode: response.StatusCode,
				Reason:     http.StatusText(response.StatusCode),
				Err:        err,
			}
		}
		return Result{}, fmt.Errorf("ws dial: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(maxSignalRMessageBytes)

	// gorilla/websocket does not make ReadMessage context-aware. Closing the
	// socket is the only reliable way to wake a blocked handshake or stream
	// read when the HTTP request is canceled.
	var writeMu sync.Mutex
	write := func(b []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		// Deadlines are absolute. Setting this once at connection startup made
		// the first 30s keepalive run after the old 15s deadline had already
		// expired, so every later heartbeat silently failed.
		_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return conn.WriteMessage(websocket.TextMessage, b)
	}
	contextStop := make(chan struct{})
	contextDone := make(chan struct{})
	var stopOnce sync.Once
	// sendStopFrame politely cancels the upstream generation ("Stop
	// generating" frame captured from the real client). Without it an aborted
	// request leaves the turn running to completion server-side, consuming the
	// per-conversation message quota for output nobody will read.
	sendStopFrame := func() {
		stopOnce.Do(func() {
			writeMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_ = conn.WriteMessage(websocket.TextMessage,
				[]byte(`{"arguments":[{}],"invocationId":"1","target":"stop","type":1}`+rs))
			writeMu.Unlock()
		})
	}
	go func() {
		defer close(contextDone)
		select {
		case <-ctx.Done():
			sendStopFrame()
			_ = conn.Close()
		case <-contextStop:
		}
	}()
	defer func() {
		close(contextStop)
		<-contextDone
	}()

	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))

	if err := write([]byte(`{"protocol":"json","version":1}` + rs)); err != nil {
		return Result{}, fmt.Errorf("handshake send: %w", err)
	}
	_, handshake, err := conn.ReadMessage()
	if err != nil {
		return Result{}, fmt.Errorf("handshake recv: %w", err)
	}
	if err := validateSignalRHandshake(handshake); err != nil {
		return Result{}, fmt.Errorf("handshake recv: %w", err)
	}

	// Keep the connection alive while upstream silently reasons for long
	// stretches. ChatHub expects periodic pings; without them intermediaries
	// may reap an idle connection with a 1006 abnormal closure.
	pingInterval := c.PingInterval
	if pingInterval <= 0 {
		pingInterval = 15 * time.Second
	}
	pingStop := make(chan struct{})
	pingDone := make(chan struct{})
	pingErr := make(chan error, 1)
	go func() {
		defer close(pingDone)
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				// Keep both layers alive. ChatHub expects its SignalR application
				// heartbeat, while some HTTP/SOCKS intermediaries only refresh their
				// idle timer when they see a standard WebSocket control frame.
				writeMu.Lock()
				controlErr := conn.WriteControl(websocket.PingMessage, []byte("m365-keepalive"), time.Now().Add(2*time.Second))
				writeMu.Unlock()
				if controlErr != nil {
					select {
					case pingErr <- controlErr:
					default:
					}
					_ = conn.Close()
					return
				}
				if err := write([]byte(`{"type":6}` + rs)); err != nil {
					select {
					case pingErr <- err:
					default:
					}
					// Closing the socket wakes the blocking ReadMessage below so the
					// normal retry/failover path can handle the broken connection.
					_ = conn.Close()
					return
				}
			}
		}
	}()
	defer func() {
		close(pingStop)
		<-pingDone
	}()

	payload := chatPayload(req.Text, req.SessionID, req.ConversationID, requestID, req.Tone, firstTurn, req.Attachments, req.Tools, req.ToolChoice)
	if err := write([]byte(payload)); err != nil {
		return Result{}, fmt.Errorf("chat send: %w", err)
	}

	var deltas []string
	var streamed strings.Builder
	answers := newAnswerStream()
	traceFrames := os.Getenv("M365_TRACE_PROTOCOL") == "1"
	emitDelta := func(d string) error {
		if d == "" {
			return nil
		}
		streamed.WriteString(d)
		deltas = append(deltas, d)
		if onDelta != nil {
			committed, err := onDelta(d)
			if err != nil {
				return err
			}
			if committed {
				// Only bytes/events delivered outside this client make replay
				// unsafe. Synchronous Chat may receive partial internal deltas, but
				// callers have seen nothing and a full retry is still correct.
				*emitted = true
			}
		}
		return nil
	}
	var final string
	var throttling any
	var rawResult string
	var resultError string
	var events []json.RawMessage
	retainedEventBytes := 0
	eventsTruncated := false
	seenStreamTools := map[string]bool{}
	partialResult := func() Result {
		text := final
		if strings.TrimSpace(text) == "" {
			text = answers.bestText()
		}
		if strings.TrimSpace(text) == "" {
			text = strings.Join(deltas, "")
		}
		text = scrubCitations(scrubNarration(text))
		return Result{
			Text: text, FullText: text,
			ConversationID: req.ConversationID, SessionID: req.SessionID, RequestID: requestID,
			Throttling: throttling, RawResult: rawResult,
			Events: events, EventsTruncated: eventsTruncated,
			Normalized: NormalizeEvents(events), Images: imageURLs(events),
		}
	}

	// The HTTP layer owns the request budget (currently configurable up to an
	// hour). Do not silently cap a configured long task at ten minutes here.
	deadline := time.Now().Add(10 * time.Minute)
	if ctxDeadline, ok := ctx.Deadline(); ok {
		deadline = ctxDeadline
	}
	var finalFrameDeadline time.Time
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		default:
		}
		readDeadline := deadline
		if !finalFrameDeadline.IsZero() && finalFrameDeadline.Before(readDeadline) {
			readDeadline = finalFrameDeadline
		}
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(readDeadline) {
			readDeadline = ctxDeadline
		}
		_ = conn.SetReadDeadline(readDeadline)
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return partialResult(), ctx.Err()
			}
			select {
			case pingWriteErr := <-pingErr:
				return partialResult(), fmt.Errorf("signalr ping: %w", pingWriteErr)
			default:
			}
			// Never convert a timeout or dropped WebSocket into a successful
			// partial response. A response is complete only after SignalR type 3.
			return partialResult(), fmt.Errorf("ws read before completion: %w", err)
		}
		for _, part := range strings.Split(string(msg), rs) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			totalEventCount++
			totalEventBytes += len(part)
			rawEvent := json.RawMessage(append([]byte(nil), part...))
			if len(rawEvent) <= maxRetainedEventBytes {
				events = append(events, rawEvent)
				retainedEventBytes += len(rawEvent)
				for len(events) > maxRetainedEventCount || retainedEventBytes > maxRetainedEventBytes {
					retainedEventBytes -= len(events[0])
					events[0] = nil
					events = events[1:]
					eventsTruncated = true
				}
			} else {
				eventsTruncated = true
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(part), &obj); err != nil {
				return Result{}, fmt.Errorf("signalr protocol: invalid JSON frame")
			}
			if traceFrames {
				preview := part
				if len(preview) > 8192 {
					preview = preview[:8192] + "..."
				}
				log.Printf("[chathub-frame] request=%s bytes=%d json=%s", requestID, len(part), preview)
			}
			t, _ := obj["type"].(float64)
			target, _ := obj["target"].(string)
			frameType := int(t)
			if frameType == 1 && target == "update" {
				updateFrameCount++
			} else if frameType == 2 {
				resultFrameCount++
			} else if frameType == 3 {
				completionFrameCount++
			} else if frameType != 6 && frameType != 7 {
				unknownFrameCount++
			}

			// SignalR ping
			if int(t) == 6 {
				if err := write([]byte(`{"type":6}` + rs)); err != nil {
					return Result{}, fmt.Errorf("signalr ping reply: %w", err)
				}
				continue
			}

			if int(t) == 1 && target == "update" {
				args, _ := obj["arguments"].([]any)
				for _, raw := range args {
					arg, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					msgs, _ := arg["messages"].([]any)
					cursorID := cursorMessageID(arg)
					if onEvent != nil {
						for _, ev := range extractToolEvents(arg, seenStreamTools) {
							committed, err := onEvent(ev)
							if err != nil {
								return Result{}, err
							}
							if committed {
								*emitted = true
							}
						}

						for _, ev := range classifyUpdateMessages(msgs) {
							ev.Raw = eventRaw(arg)
							if ev.Kind != "text" {
								committed, err := onEvent(ev)
								if err != nil {
									return Result{}, err
								}
								if committed {
									*emitted = true
								}
							}
						}
					}
					disengaged := false
					for _, mraw := range msgs {
						m, _ := mraw.(map[string]any)
						mt, _ := m["messageType"].(string)
						if mt == "Disengaged" {
							// The safety filter's refusal frame: empty content,
							// offense usually "None". Retrying the same prompt
							// re-disengages and burns conversation quota, so
							// surface it as its own error class.
							disengaged = true
						}
					}
					if disengaged {
						return Result{}, fmt.Errorf("%w", ErrDisengaged)
					}
					if thr, ok := arg["throttling"]; ok {
						throttling = thr
					}
					if msgs, ok := arg["messages"].([]any); ok {
						visibleIndex := -1
						cursorMatched := false
						for i, mraw := range msgs {
							m, _ := mraw.(map[string]any)
							if !visibleAnswerMessage(m) || isCodeInterpreterArtifact(stringValue(m["text"])) {
								continue
							}
							messageID := stringValue(m["messageId"])
							if cursorID == "" {
								visibleIndex = i
								continue
							}
							if messageID == cursorID {
								visibleIndex = i
								cursorMatched = true
							} else if messageID == "" && !cursorMatched {
								visibleIndex = i
							}
						}
						for i, mraw := range msgs {
							m, ok := mraw.(map[string]any)
							if !ok {
								continue
							}
							text, _ := m["text"].(string)
							// dea_violation is the classifier score that correlates
							// with the Disengaged filter firing (clean tool calls
							// ~1e-8, prose ~1e-6, refusals above ~2e-3). Log a
							// warning past the threshold so operators can rotate
							// the account before it starts refusing outright.
							if strings.EqualFold(stringValue(m["author"]), "bot") {
								if scores, ok := m["scores"].([]any); ok {
									for _, sraw := range scores {
										s, _ := sraw.(map[string]any)
										if comp, _ := s["component"].(string); comp == "dea_violation" {
											if score, ok := s["score"].(float64); ok && score > 2e-3 {
												log.Printf("[chathub] dea_violation score %.4g approaching Disengage threshold (conversation %s)", score, req.ConversationID)
											}
										}
									}
								}
							}
							if i == visibleIndex && visibleAnswerMessage(m) && !isCodeInterpreterArtifact(text) {
								traceProtocolText(traceFrames, requestID, "bot.text", text)
								messageID := stringValue(m["messageId"])
								if messageID == "" {
									messageID = cursorID
								}
								turnState := strings.TrimSpace(stringValue(m["turnState"]))
								last, _ := arg["isLastUpdate"].(bool)
								tail := answers.observeSnapshot(messageID, scrubCitations(text), last || strings.EqualFold(turnState, "Completed"))
								if err := emitDelta(tail); err != nil {
									return Result{}, err
								}
							}
						}
					}
					if cursorID != "" {
						answers.selectCursor(cursorID)
					}
					if w, ok := arg["writeAtCursor"].(string); ok && w != "" {
						traceProtocolText(traceFrames, requestID, "writeAtCursor", w)
						if delta, visible := answers.appendAtCursor(scrubCitations(w)); visible {
							if err := emitDelta(delta); err != nil {
								return Result{}, err
							}
						}
					}
					// isLastUpdate is the upstream's explicit final-frame signal.
					// Don't return a hand-built Result here — the type:2/type:3
					// frames that follow carry throttling and result-error data
					// the completion path needs. Instead shorten the remaining
					// read window so a lost terminator can't idle the turn.
					if last, ok := arg["isLastUpdate"].(bool); ok && last {
						finalFrameTimeout := c.FinalFrameTimeout
						if finalFrameTimeout <= 0 {
							finalFrameTimeout = defaultFinalFrameTimeout
						}
						// Restart the grace period whenever upstream advances the
						// terminal handshake. Keeping the earliest deadline made a
						// later type:2 result inherit time already spent waiting after
						// isLastUpdate and cut off a valid, still-progressing response.
						finalFrameDeadline = time.Now().Add(finalFrameTimeout)
					}
				}
				continue
			}

			if int(t) == 2 {
				finalFrameTimeout := c.FinalFrameTimeout
				if finalFrameTimeout <= 0 {
					finalFrameTimeout = defaultFinalFrameTimeout
				}
				finalFrameDeadline = time.Now().Add(finalFrameTimeout)
				item, _ := obj["item"].(map[string]any)
				if item != nil {
					if thr, ok := item["throttling"]; ok {
						throttling = thr
					}
					if res, ok := item["result"].(map[string]any); ok {
						rawResult, _ = res["value"].(string)
						if msg, ok := res["message"].(string); ok {
							final = msg
							traceProtocolText(traceFrames, requestID, "result.message", msg)
						}
						if rawResult != "" && resultError == "" {
							var rv map[string]any
							if json.Unmarshal([]byte(rawResult), &rv) == nil {
								if em, ok := rv["error"].(map[string]any); ok {
									if emsg, ok := em["message"].(string); ok && emsg != "" {
										resultError = emsg
									} else if es, ok := em["code"].(string); ok && es != "" {
										resultError = es
									}
								}
								if emsg, ok := rv["message"].(string); ok && emsg != "" {
									resultError = emsg
								}
							}
						}
					}
				}
				// completion frame often follows; keep reading a bit but we already have content
				continue
			}

			if int(t) == 3 {
				noContent := strings.TrimSpace(final) == "" && strings.TrimSpace(answers.bestText()) == "" && len(deltas) == 0
				if noContent && quotaExhausted(throttling) {
					return Result{}, quotaRateLimitError()
				}
				if errValue, ok := obj["error"]; ok && errValue != nil {
					switch value := errValue.(type) {
					case string:
						if strings.TrimSpace(value) != "" {
							return Result{}, fmt.Errorf("chathub completion error: %s", value)
						}
					default:
						return Result{}, fmt.Errorf("chathub completion error: %v", value)
					}
				}
				if resultError != "" {
					return Result{}, fmt.Errorf("chathub upstream error: %s", resultError)
				}
				// end of stream
				text := final
				if text == "" {
					text = answers.bestText()
				}
				if text == "" {
					text = strings.Join(deltas, "")
				}
				text = scrubCitations(scrubNarration(text))
				if strings.TrimSpace(text) == "" {
					if quotaExhausted(throttling) {
						return Result{}, quotaRateLimitError()
					}
					return Result{}, fmt.Errorf("chathub returned no content")
				}
				return Result{
					Text: text,
					// The type-2 result message or final visible snapshot is
					// authoritative. Streamed bytes can contain an unretractable old
					// snapshot after an upstream rewrite and must not win by length.
					FullText:        text,
					ConversationID:  req.ConversationID,
					SessionID:       req.SessionID,
					RequestID:       requestID,
					Throttling:      throttling,
					RawResult:       rawResult,
					Events:          events,
					EventsTruncated: eventsTruncated,
					Normalized:      NormalizeEvents(events),
					Images:          imageURLs(events),
				}, nil
			}

			if int(t) == 7 {
				if reason, _ := obj["error"].(string); strings.TrimSpace(reason) != "" {
					return partialResult(), fmt.Errorf("chathub closed before completion: %s", reason)
				}
				return partialResult(), fmt.Errorf("chathub closed before completion")
			}
		}
	}

	// Reaching the overall deadline without a SignalR completion frame is
	// an incomplete upstream response. Do not return accumulated deltas as if
	// they were a successful, finished answer.
	return partialResult(), fmt.Errorf("chathub response deadline exceeded before completion")
}

func validateSignalRHandshake(raw []byte) error {
	acknowledged := false
	for _, part := range strings.Split(string(raw), rs) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(part), &frame); err != nil {
			return fmt.Errorf("invalid SignalR handshake JSON")
		}
		if message, _ := frame["error"].(string); strings.TrimSpace(message) != "" {
			return fmt.Errorf("SignalR handshake rejected")
		}
		if len(frame) == 0 {
			acknowledged = true
			continue
		}
		return fmt.Errorf("unexpected SignalR handshake frame")
	}
	if !acknowledged {
		return fmt.Errorf("missing SignalR handshake acknowledgement")
	}
	return nil
}

func chatHubErrorStage(err error) string {
	if err == nil {
		return "none"
	}
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.Canceled):
		return "client_cancel"
	case errors.Is(err, context.DeadlineExceeded):
		return "request_deadline"
	case strings.Contains(message, "proxy dialer"):
		return "proxy_config"
	case strings.Contains(message, "ws dial"):
		return "websocket_dial"
	case strings.Contains(message, "handshake"):
		return "signalr_handshake"
	case strings.Contains(message, "chat send"):
		return "request_send"
	case strings.Contains(message, "signalr ping"):
		return "keepalive"
	case strings.Contains(message, "signalr protocol"):
		return "signalr_protocol"
	case strings.Contains(message, "ws read"):
		return "stream_read"
	case IsDisengagedError(err):
		return "policy_refusal"
	case strings.Contains(message, "rate limit") || strings.Contains(message, "quota"):
		return "throttling"
	case strings.Contains(message, "completion") || strings.Contains(message, "no content") || strings.Contains(message, "response deadline"):
		return "completion"
	default:
		return "unknown"
	}
}

func IsDisengagedError(err error) bool {
	return errors.Is(err, ErrDisengaged)
}

func buildWSURL(acc Account, sessionID, conversationID, requestID string) (string, error) {
	return buildWSURLForBase(wsBase, acc, sessionID, conversationID, requestID)
}

func buildWSURLForBase(base string, acc Account, sessionID, conversationID, requestID string) (string, error) {
	if strings.TrimSpace(base) == "" {
		base = wsBase
	}
	q := url.Values{}
	q.Set("chatsessionid", requestID)
	q.Set("clientrequestid", requestID)
	q.Set("X-SessionId", sessionID)
	q.Set("ConversationId", conversationID)
	q.Set("access_token", acc.AccessToken)
	q.Set("variants", variants)
	// source must keep quotes like the browser probe
	q.Set("source", `"officeweb"`)
	q.Set("product", "Office")
	q.Set("agentHost", "Bizchat.FullScreen")
	q.Set("licenseType", "Starter")
	q.Set("agent", "web")
	q.Set("scenario", "OfficeWebIncludedCopilot")

	// url.Values encodes quotes; probe used safe='",' so keep quotes unescaped-ish.
	// Gorilla/url will encode " to %22 which MS accepts.
	u := fmt.Sprintf("%s/%s@%s?%s", strings.TrimRight(base, "/"), acc.OID, acc.TID, q.Encode())
	return u, nil
}

// allowedMessageTypes mirrors the full subscription list captured from live
// m365.cloud.microsoft sessions (captured browser-protocol reference, 29 entries). The original
// two-entry list never subscribed to "Disengaged" — the safety filter's
// refusal frame arrived as nothing at all — and omitted the code-interpreter,
// memory and plugin-lifecycle types. Unsubscribed types can change upstream
// behaviour rather than being silently dropped.
var chatAllowedMessageTypes = []string{
	"Chat", "Suggestion", "InternalSearchQuery", "Disengaged",
	"InternalLoaderMessage", "Progress", "GeneratedCode", "RenderCardRequest",
	"AdsQuery", "SemanticSerp", "GenerateContentQuery", "GenerateGraphicArt",
	"SearchQuery", "ConfirmationCard", "AuthError", "DeveloperLogs",
	"TriggerPlugin", "HintInvocation", "MemoryUpdate", "EndOfRequest",
	"TriggerConfirmation", "ResumeInvokeAction", "ResumeUserInputRequest",
	"TriggerUserInputRequest", "EscapeHatch", "TriggerPluginAuth",
	"ResumePluginAuth", "SideBySide", "ReferencesListComplete",
	"SwitchRespondingEndpoint",
}

// chatOptionsSets mirrors the live client's capability switches (captured browser-protocol
// reference, 21 entries): the server-side Python sandbox and its chart
// patches, file upload via OneDrive, memory and custom instructions (both of
// which affect answer quality), batched token processing and the flux image
// pipeline. The previous 7-entry list left most of these off.
var chatOptionsSets = []string{
	"search_result_progress_messages_with_search_queries",
	"cwc_flux_image",
	"cwc_code_interpreter",
	"cwc_code_interpreter_amsfix",
	"cwcfluxgptv",
	"flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch",
	"cwc_code_interpreter_citation_fix",
	"code_interpreter_interactive_charts",
	"cwc_code_interpreter_interactive_charts_inline_image",
	"code_interpreter_matplotlib_patching",
	"cwc_fileupload_odb",
	"update_memory_plugin",
	"add_custom_instructions",
	"cwc_flux_v3",
	"flux_v3_progress_messages",
	"enable_batch_token_processing",
	"enable_gg_gpt",
	"flux_v3_image_gen_enable_dimensions",
	"flux_v3_image_gen_enable_icon_dimensions",
	"flux_v3_image_gen_enable_system_text_with_params",
	"flux_v3_image_gen_enable_designer_dimensions_meta_prompting_in_system_prompts",
}

func chatPayload(text, sessionID, conversationID, requestID, tone string, firstTurn bool, attachments []Attachment, tools []Tool, toolChoice any) string {
	text = toolProtocolPrompt(text, tools, toolChoice)
	chat := map[string]any{
		"arguments": []any{
			map[string]any{
				"source":              "officeweb",
				"clientCorrelationId": requestID,
				"sessionId":           sessionID,
				"optionsSets":         chatOptionsSets,
				"streamingMode":       "ConciseWithPadding",
				"spokenTextMode":      "None",
				"options":             map[string]any{},
				"sliceIds":            []any{},
				"threadLevelGptId":    map[string]any{},
				"conversationId":      conversationID,
				"traceId":             requestID,
				"isStartOfSession":    firstTurn,
				"productThreadType":   "Office",
				"clientInfo": map[string]any{
					"clientPlatform":    "mcmcopilot-web",
					"clientAppName":     "Office",
					"clientEntrypoint":  "mcmcopilot-officeweb",
					"clientSessionId":   sessionID,
					"ProductCategory":   "Chat",
					"clientAppType":     "Web",
					"productEntryPoint": "ChatPanel",
					"deviceOS":          "Windows",
					"deviceType":        "Desktop",
				},
				"tone":                      tone,
				"isSbsSupported":            true,
				"renderReferencesBehindEOS": true,
				"message": map[string]any{
					"author":      "user",
					"attachments": attachments,
					"inputMethod": "Keyboard",
					"text":        text,
					"entityAnnotationTypes": []string{
						"People", "File", "Event", "Email", "TeamsMessage",
					},
					"requestId": requestID,
					"locationInfo": map[string]any{
						"timeZoneOffset": 8,
						"timeZone":       "Asia/Shanghai",
					},
					"locale":         "en-US",
					"messageType":    "Chat",
					"experienceType": "Default",
					"adaptiveCards":  []any{},
				},
				"plugins":    clientPlugins(tools),
				"toolChoice": toolChoice,
			},
		},
		"invocationId": "0",
		"target":       "chat",
		"type":         4,
	}
	metrics := map[string]any{
		"arguments": []any{
			map[string]any{
				"Timestamps": map[string]string{
					"ConnectionStart":       "",
					"UserInputStart":        "",
					"ConnectionEstablished": "",
					"UserInputSubmit":       "",
				},
			},
		},
		"target": "Metrics",
		"type":   1,
	}
	b1, _ := json.Marshal(chat)
	b2, _ := json.Marshal(metrics)
	return string(b1) + rs + string(b2) + rs
}
