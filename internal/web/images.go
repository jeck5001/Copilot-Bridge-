package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
	"net/http"
	"strings"
	"time"
)

type imageGenerationRequest struct {
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	Model          string `json:"model"`
	AccountID      string `json:"accountId"`
	User           string `json:"user"`
}

func (s *Server) imageGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var b imageGenerationRequest
	if json.NewDecoder(r.Body).Decode(&b) != nil || strings.TrimSpace(b.Prompt) == "" {
		http.Error(w, `{"error":{"message":"prompt is required","type":"invalid_request_error"}}`, 400)
		return
	}
	if b.N <= 0 {
		b.N = 1
	}
	if b.N > 4 {
		http.Error(w, "n must be between 1 and 4", 400)
		return
	}
	if b.ResponseFormat != "" && !strings.EqualFold(b.ResponseFormat, "url") && !strings.EqualFold(b.ResponseFormat, "b64_json") {
		http.Error(w, `{"error":{"message":"response_format must be url or b64_json","type":"invalid_request_error"}}`, 400)
		return
	}
	requestedAccountID := strings.TrimSpace(b.AccountID)
	acc, switched, err := s.resolveRequestAccount(requestedAccountID, requestedAccountID != "")
	if err != nil {
		if errors.Is(err, errUpstreamCircuitOpen) {
			writeAccountResolutionError(w, err)
		} else {
			writeOpenAIError(w, http.StatusConflict, "account_isolated", err.Error())
		}
		return
	}
	if acc.OID == "" || acc.TID == "" {
		acc.OID, acc.TID = extractOIDTID(acc.AccessToken)
	}
	if acc.OID == "" || acc.TID == "" {
		http.Error(w, "account missing oid/tid", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ImageTimeoutSeconds)*time.Second)
	defer cancel()
	account := chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID, Proxy: acc.Proxy}
	req := chathub.Request{Text: "Generate an image: " + b.Prompt, Tone: "magic"}
	res, err := s.chatActive(ctx, acc.ID, account, req)
	if err == nil && len(res.Images) == 0 {
		err = fmt.Errorf("upstream returned no image resource")
	}
	markTerminalFailure := func(failure error) {
		if switched {
			s.recordAccountFailureWithoutAdvance(acc.ID, failure)
			return
		}
		s.markAccountResult(acc.ID, failure)
	}
	if r.Context().Err() == nil && shouldFailoverAccount(false, switched, false, err, res) {
		failure := err
		if failure == nil {
			failure = strictQuotaExhaustedError()
		}
		advanced := s.markAccountResult(acc.ID, failure)
		if advanced {
			if next, nextErr := s.nextHealthyAccount(acc.ID); nextErr == nil {
				if next.OID == "" || next.TID == "" {
					next.OID, next.TID = extractOIDTID(next.AccessToken)
				}
				if next.OID != "" && next.TID != "" {
					acc = next
					account = chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID, Proxy: next.Proxy}
					switched = true
					res, err = s.chatActive(ctx, acc.ID, account, req)
					if err == nil && len(res.Images) == 0 {
						err = fmt.Errorf("upstream returned no image resource")
					}
				}
			}
		}
	}
	if err != nil {
		if r.Context().Err() == nil {
			markTerminalFailure(err)
		}
		http.Error(w, "upstream error", 502)
		return
	}
	if quotaFailureWithoutContent(res) {
		markTerminalFailure(strictQuotaExhaustedError())
		writeOpenAIError(w, http.StatusBadGateway, "upstream_throttled", "Microsoft Copilot quota exhausted")
		return
	}
	if len(res.Images) == 0 {
		http.Error(w, `{"error":{"message":"upstream returned no image resource","type":"upstream_error"}}`, 502)
		return
	}
	s.markAccountSuccess(acc.ID, res.Throttling)
	images := res.Images
	if len(images) > b.N {
		images = images[:b.N]
	}
	data := make([]map[string]string, 0, len(images))
	for _, u := range images {
		if strings.EqualFold(b.ResponseFormat, "b64_json") {
			if !strings.HasPrefix(u, "data:image/") {
				http.Error(w, `{"error":{"message":"upstream returned URL, not b64_json","type":"unsupported_response_format"}}`, 502)
				return
			}
			data = append(data, map[string]string{"b64_json": strings.SplitN(u, ",", 2)[1]})
		} else {
			data = append(data, map[string]string{"url": u})
		}
	}
	jsonOut(w, map[string]any{"created": time.Now().Unix(), "data": data, "m365": map[string]any{"conversationId": res.ConversationID, "sessionId": res.SessionID, "images": images}})
}
