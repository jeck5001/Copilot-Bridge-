package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

func (s *Server) chatStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body chatBody
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(firstNonEmpty(body.Message, body.Prompt))
	if text == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	explicitAccountID := strings.TrimSpace(body.AccountID)
	activeAccountID := explicitAccountID
	if body.SessionKey != "" {
		if v, ok := s.sessions.get(body.SessionKey); ok {
			if activeAccountID == "" {
				activeAccountID = v.AccountID
			}
			if activeAccountID != "" && activeAccountID == v.AccountID {
				body.ConversationID = firstNonEmpty(body.ConversationID, v.ConversationID)
				body.SessionID = firstNonEmpty(body.SessionID, v.SessionID)
			}
		}
	}
	acc, preflightSwitched, err := s.resolveRequestAccount(activeAccountID, explicitAccountID != "")
	if err != nil {
		if errors.Is(err, errUpstreamCircuitOpen) {
			writeAccountResolutionError(w, err)
		} else {
			http.Error(w, "bad request", http.StatusBadRequest)
		}
		return
	}
	if preflightSwitched {
		activeAccountID = acc.ID
		body.ConversationID = ""
		body.SessionID = ""
	}
	if acc.OID == "" || acc.TID == "" {
		if o, t := extractOIDTID(acc.AccessToken); o != "" {
			acc.OID, acc.TID = o, t
		}
	}
	if acc.OID == "" || acc.TID == "" {
		http.Error(w, "account missing oid/tid", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	if err := sseRaw(r.Context(), w, flusher, ": connected\n\n"); err != nil {
		return
	}

	req := chathub.Request{
		Text: text, Tone: body.Tone, ConversationID: body.ConversationID, SessionID: body.SessionID, Attachments: body.Attachments,
	}
	account := chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID, Proxy: acc.Proxy}
	index := 0
	visibleOutput := false
	downstreamWriteFailed := false
	handleEvent := func(event chathub.StreamEvent) (bool, error) {
		payload := map[string]any{
			"index": index, "type": "chathub.event", "kind": event.Kind,
			"text": event.Text, "messageType": event.MessageType,
			"contentType": event.ContentType, "toolName": event.ToolName,
			"arguments": event.Arguments, "raw": event.Raw,
		}
		if err := writeSSE(w, "event", payload); err != nil {
			downstreamWriteFailed = true
			return false, err
		}
		index++
		visibleOutput = true
		return true, nil
	}
	res, err := s.chatActiveWithEvents(ctx, acc.ID, account, req, handleEvent)
	switched := preflightSwitched
	markTerminalFailure := func(failure error) {
		if switched {
			s.recordAccountFailureWithoutAdvance(acc.ID, failure)
			return
		}
		s.markAccountResult(acc.ID, failure)
	}
	if !downstreamWriteFailed && r.Context().Err() == nil && shouldFailoverAccount(explicitAccountID != "", switched, visibleOutput, err, res, body.Tools) {
		failure := err
		if failure == nil {
			failure = strictQuotaExhaustedError()
		}
		advanced := s.markAccountResult(acc.ID, failure)
		if advanced {
			if next, nextErr := s.nextHealthyAccount(acc.ID); nextErr == nil {
				acc = next
				if acc.OID == "" || acc.TID == "" {
					if oid, tid := extractOIDTID(acc.AccessToken); oid != "" {
						acc.OID, acc.TID = oid, tid
					}
				}
				account = chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID, Proxy: acc.Proxy}
				req.ConversationID = ""
				req.SessionID = ""
				switched = true
				res, err = s.chatActiveWithEvents(ctx, acc.ID, account, req, handleEvent)
			}
		}
	}
	if err != nil {
		if !downstreamWriteFailed && r.Context().Err() == nil {
			markTerminalFailure(err)
			_ = writeSSE(w, "error", map[string]any{"type": "upstream_error", "message": err.Error()})
		}
		return
	}
	if quotaFailureWithoutContent(res, body.Tools) && !visibleOutput {
		markTerminalFailure(strictQuotaExhaustedError())
		_ = writeSSE(w, "error", map[string]any{"type": "upstream_error", "code": "upstream_throttled", "message": "Microsoft Copilot quota exhausted"})
		return
	}
	s.markAccountSuccess(acc.ID, res.Throttling)
	if body.SessionKey != "" {
		if err := s.persistSession(body.SessionKey, acc.ID, text, res); err != nil {
			_ = writeSSE(w, "error", map[string]any{"type": "session_persistence_error", "message": "response state could not be persisted"})
			return
		}
	}

	s.recordTokens(acc.ID, text, res.FullText)
	if err := writeSSE(w, "done", map[string]any{
		"type": "done", "text": res.Text,
		"conversationId": res.ConversationID, "sessionId": res.SessionID, "requestId": res.RequestID,
		"throttling": res.Throttling,
	}); err != nil {
		return
	}
}

// writeSSE emits one SSE frame and flushes; a write error (client gone, deadline
// exceeded) is returned so the caller can abort instead of blocking a goroutine
// against a dead socket. A per-write deadline bounds how long a slow client can
// stall the handler.
func writeSSE(w http.ResponseWriter, name string, value any) error {
	b, _ := json.Marshal(value)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
