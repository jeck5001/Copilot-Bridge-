package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/vipamess/Copilot-Bridge-/internal/auth"
	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
	"github.com/vipamess/Copilot-Bridge-/internal/proxy"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type pendingPKCE struct {
	Verifier string
	Created  time.Time
}

type keyedResponseLock struct {
	mu   sync.Mutex
	refs int
}

var toolRouterPhaseTimeout = 120 * time.Second

type Server struct {
	mu                      sync.Mutex
	accountRouteMu          sync.Mutex
	activeAccountID         string
	activeAccountGeneration uint64
	nextAccountLeaseID      uint64
	activeAccountLeases     map[uint64]context.CancelFunc
	sharedFailureAccounts   map[string]time.Time
	upstreamCircuitUntil    time.Time
	accountRoutePath        string
	tokens                  *auth.Store
	pkce                    map[string]pendingPKCE
	chat                    *chathub.Client
	sessions                *sessionStore
	adminPassword           string
	adminSessions           map[string]time.Time
	mustChangePassword      bool
	loginAttempts           map[string]loginAttempt
	apiKeys                 *apiKeyStore
	debug                   *debugStore
	settings                *settingsStore
	accountStats            map[string]int64
	accountTokenIn          map[string]int64
	accountTokenOut         map[string]int64
	statsPath               string
	statsDirty              bool
	accountPool             *accountHealth
	accountPoolOnce         sync.Once
	responseSessionMu       sync.Mutex
	responseSessionLocks    map[string]*keyedResponseLock
	policyRefusalMu         sync.Mutex
	policyRefusals          map[[32]byte]time.Time
}

func New() (*Server, error) {
	store, err := auth.OpenStore("")
	if err != nil {
		return nil, err
	}
	password, mustChange, err := loadAdminPassword()
	if err != nil {
		return nil, err
	}
	apiKeys, err := openAPIKeys()
	if err != nil {
		return nil, fmt.Errorf("open API key store: %w", err)
	}
	sessions, err := openSessionStore()
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	settings, err := openSettingsStore()
	if err != nil {
		return nil, fmt.Errorf("open settings store: %w", err)
	}
	s := &Server{
		tokens:                store,
		pkce:                  map[string]pendingPKCE{},
		chat:                  chathub.NewClient(),
		sessions:              sessions,
		adminPassword:         password,
		adminSessions:         map[string]time.Time{},
		mustChangePassword:    mustChange,
		loginAttempts:         map[string]loginAttempt{},
		apiKeys:               apiKeys,
		debug:                 openDebugStore(),
		settings:              settings,
		accountStats:          make(map[string]int64),
		accountTokenIn:        make(map[string]int64),
		accountTokenOut:       make(map[string]int64),
		statsPath:             filepath.Join(filepath.Dir(auth.CachePath()), "stats.json"),
		accountRoutePath:      filepath.Join(filepath.Dir(auth.CachePath()), "account-route.json"),
		activeAccountLeases:   make(map[uint64]context.CancelFunc),
		sharedFailureAccounts: make(map[string]time.Time),
		policyRefusals:        make(map[[32]byte]time.Time),
	}
	if err := s.initializeAccountRouter(); err != nil {
		return nil, fmt.Errorf("initialize account router: %w", err)
	}
	if err := s.loadStats(); err != nil {
		return nil, fmt.Errorf("load statistics: %w", err)
	}
	go s.statsSaver()
	return s, nil
}

func (s *Server) Routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/api/admin/login", s.adminLogin)
	m.HandleFunc("/api/admin/logout", s.adminLogout)
	m.HandleFunc("/api/admin/session", s.adminSession)
	m.HandleFunc("/api/admin/change-password", s.adminChangePassword)
	m.HandleFunc("/api/admin/keys", s.adminKeys)
	m.HandleFunc("/api/admin/settings", s.adminSettings)
	m.HandleFunc("/api/admin/session-cache", s.adminSessionCache)
	m.HandleFunc("/api/admin/debug/logs", s.debugList)
	m.HandleFunc("/api/admin/debug/detail", s.debugDetail)
	m.HandleFunc("/api/health", s.health)
	m.HandleFunc("/api/accounts", s.accounts)
	m.HandleFunc("/api/accounts/refresh", s.refreshAccount)
	m.HandleFunc("/api/accounts/delete", s.deleteAccount)
	m.HandleFunc("/api/accounts/proxy", s.updateAccountProxy)
	m.HandleFunc("/api/admin/test-proxy", s.testProxy)
	m.HandleFunc("/api/admin/test-all-proxies", s.testAllProxies)
	m.HandleFunc("/api/admin/reset-stats", s.resetStatsHandler)
	m.HandleFunc("/api/auth/start", s.startPKCE)
	m.HandleFunc("/api/auth/callback", s.callbackPKCE)
	m.HandleFunc("/api/chat", s.chatOnce)
	m.HandleFunc("/api/chat/stream", s.chatStream)
	m.HandleFunc("/api/conversations", s.conversations)
	m.HandleFunc("/api/conversations/delete", s.deleteConversation)
	m.HandleFunc("/v1/models", s.openaiModels)
	m.HandleFunc("/v1/models/", s.openaiModelDetail)
	m.HandleFunc("/v1/chat/completions", s.openaiChat)
	m.HandleFunc("/v1/responses", s.responses)
	m.HandleFunc("/v1/realtime", s.realtimeUnsupported)
	m.HandleFunc("/v1/realtime/", s.realtimeUnsupported)
	m.HandleFunc("/v1/messages", s.anthropicMessages)
	m.HandleFunc("/v1/images/generations", s.imageGenerations)
	m.HandleFunc("/admin.js", s.consoleScript)
	m.HandleFunc("/", s.rootPage)
	return requestID(securityHeaders(requestBodyLimit(sseKeepaliveMiddleware(s.adminMiddleware(s.debugMiddleware(m))))))
}

func (s *Server) realtimeUnsupported(w http.ResponseWriter, _ *http.Request) {
	writeOpenAIError(w, http.StatusNotImplemented, "realtime_not_supported", "this Microsoft 365 bridge supports text streaming but does not provide the OpenAI Realtime audio WebSocket protocol")
}

func (s *Server) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/login" || r.URL.Path == "/api/admin/session" || r.URL.Path == "/api/admin/change-password" || r.URL.Path == "/api/admin/logout" || r.URL.Path == "/" || r.URL.Path == "/admin.js" || r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			if !s.validAPIKey(r) {
				http.Error(w, `{"error":{"message":"valid API key required","type":"auth_error"}}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		s.mu.Lock()
		passwordConfigured := s.adminPassword != ""
		s.mu.Unlock()
		if !passwordConfigured {
			http.Error(w, `{"error":{"message":"administrator password is not configured","type":"configuration_error"}}`, http.StatusServiceUnavailable)
			return
		}
		if !s.validAdminSession(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
			return
		}
		s.mu.Lock()
		mustChange := s.mustChangePassword
		s.mu.Unlock()
		if mustChange && r.URL.Path != "/api/admin/change-password" && r.URL.Path != "/api/admin/logout" {
			writeOpenAIError(w, http.StatusForbidden, "password_change_required", "administrator password must be changed before using the console")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validAdminSession(r *http.Request) bool {
	c, err := r.Cookie("m365_admin_session")
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.adminSessions[c.Value]
	if !ok || time.Now().After(expires) {
		delete(s.adminSessions, c.Value)
		return false
	}
	return true
}

func cookieSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") || strings.EqualFold(r.Header.Get("X-Forwarded-Ssl"), "on") {
		return true
	}
	return strings.EqualFold(os.Getenv("M365_COOKIE_SECURE"), "true")
}

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	ip, now := clientIP(r), time.Now()
	if ok, wait := s.loginAllowed(ip, now); !ok {
		seconds := int(wait.Seconds()) + 1
		w.Header().Set("Retry-After", fmt.Sprint(seconds))
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "too many failed login attempts; try again later")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	decodeErr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	s.mu.Lock()
	password := s.adminPassword
	mustChange := s.mustChangePassword
	s.mu.Unlock()
	if decodeErr != nil || !verifyAdminPassword(password, body.Password) {
		s.recordLoginFailure(ip, now)
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "invalid administrator password")
		return
	}
	s.clearLoginFailures(ip)
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		writeOpenAIError(w, 500, "internal_error", "session failure")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.adminSessions[token] = time.Now().Add(24 * time.Hour)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "m365_admin_session", Value: token, Path: "/", HttpOnly: true, Secure: cookieSecure(r), SameSite: http.SameSiteLaxMode, MaxAge: 86400})
	jsonOut(w, map[string]any{"status": "authenticated", "must_change_password": mustChange})
}
func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("m365_admin_session"); e == nil {
		s.mu.Lock()
		delete(s.adminSessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "m365_admin_session", Path: "/", HttpOnly: true, Secure: cookieSecure(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	jsonOut(w, map[string]string{"status": "logged_out"})
}
func (s *Server) adminSession(w http.ResponseWriter, r *http.Request) {
	authenticated := s.validAdminSession(r)
	s.mu.Lock()
	mustChange := s.mustChangePassword
	s.mu.Unlock()
	jsonOut(w, map[string]bool{"authenticated": authenticated, "must_change_password": authenticated && mustChange})
}

func (s *Server) adminKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, map[string]any{"keys": s.apiKeys.list()})
	case http.MethodPost:
		var b struct {
			Name string `json:"name"`
			Days int    `json:"days"`
		}
		if json.NewDecoder(r.Body).Decode(&b) != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if strings.TrimSpace(b.Name) == "" {
			b.Name = "API key"
		}
		rec, raw, e := s.apiKeys.create(b.Name, b.Days)
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		jsonOut(w, map[string]any{"key": raw, "record": rec})
	case http.MethodPatch:
		var b struct {
			ID   string `json:"id"`
			Days int    `json:"days"`
		}
		if json.NewDecoder(r.Body).Decode(&b) != nil || b.ID == "" {
			http.Error(w, "bad json", 400)
			return
		}
		ok, err := s.apiKeys.setExpiry(b.ID, b.Days)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "storage_error", "could not persist API key expiry")
			return
		}
		if !ok {
			http.Error(w, "key not found", 404)
			return
		}
		jsonOut(w, map[string]string{"status": "updated"})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		var ok bool
		var err error
		if r.URL.Query().Get("purge") == "true" {
			ok, err = s.apiKeys.remove(id)
		} else {
			ok, err = s.apiKeys.revoke(id)
		}
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "storage_error", "could not persist API key revocation")
			return
		}
		if !ok {
			http.Error(w, "key not found", 404)
			return
		}
		jsonOut(w, map[string]string{"status": "revoked"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}
func (s *Server) validAPIKey(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if raw == "" {
		v := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(v), "bearer ") {
			raw = strings.TrimSpace(v[7:])
		}
	}
	return raw != "" && s.apiKeys.valid(raw)
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	jsonOut(w, map[string]any{
		"status": "ok",
	})
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	list := s.tokens.List()
	type view struct {
		ID           string            `json:"id"`
		Email        string            `json:"email"`
		DisplayName  string            `json:"displayName,omitempty"`
		Status       string            `json:"status"`
		OID          string            `json:"oid,omitempty"`
		TID          string            `json:"tid,omitempty"`
		ExpiresAt    time.Time         `json:"expiresAt,omitempty"`
		UpdatedAt    time.Time         `json:"updatedAt,omitempty"`
		RequestCount int64             `json:"requestCount"`
		TokenIn      int64             `json:"tokenIn"`
		TokenOut     int64             `json:"tokenOut"`
		Proxy        string            `json:"proxy,omitempty"`
		Health       accountHealthView `json:"health,omitempty"`
		Position     int               `json:"position"`
		Active       bool              `json:"active"`
		Isolated     bool              `json:"isolated"`
	}
	out := make([]view, 0, len(list))
	var total int64
	var totalIn, totalOut int64
	healthMap := make(map[string]accountHealthView)
	for _, hv := range s.healthPool().Snapshot() {
		healthMap[hv.AccountID] = hv
	}
	activeAccountID := s.currentActiveAccountID()
	s.mu.Lock()
	for position, a := range list {
		cnt := s.accountStats[a.ID]
		total += cnt
		tin := s.accountTokenIn[a.ID]
		tout := s.accountTokenOut[a.ID]
		totalIn += tin
		totalOut += tout
		out = append(out, view{
			ID: a.ID, Email: a.Email, DisplayName: a.DisplayName,
			Status: a.Status, OID: a.OID, TID: a.TID,
			ExpiresAt: a.ExpiresAt, UpdatedAt: a.UpdatedAt,
			RequestCount: cnt,
			TokenIn:      tin,
			TokenOut:     tout,
			Proxy:        a.Proxy,
			Health:       healthMap[a.ID],
			Position:     position + 1,
			Active:       a.ID == activeAccountID,
			Isolated:     a.ID != activeAccountID,
		})
	}
	s.mu.Unlock()
	jsonOut(w, map[string]any{"accounts": out, "totalRequestCount": total, "totalTokenIn": totalIn, "totalTokenOut": totalOut})
}

// accountRequestCount returns the per-account request counter under the server
// lock. Reading accountStats without the lock races with the write in
// resolveAccount and triggers Go's fatal "concurrent map read and map write".
func (s *Server) accountRequestCount(id string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accountStats[id]
}

// estimateTokens approximates the token count of a string with a simple
// heuristic: ~4 ASCII chars per token and ~1 token per non-ASCII (e.g. CJK)
// character. M365 Copilot does not expose real token usage, so this value is
// an estimate only and must never be treated as billing-accurate.
func estimateTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

// recordTokens estimates and accumulates input/output token usage for an
// account. It reuses the server mutex that guards the other per-account
// counters so concurrent requests cannot race on the maps.
func (s *Server) recordTokens(id, inputText, outputText string) {
	if id == "" {
		return
	}
	in := int(countTokens("m365-copilot", inputText))
	out := int(countTokens("m365-copilot", outputText))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountTokenIn[id] += int64(in)
	s.accountTokenOut[id] += int64(out)
	s.statsDirty = true
}

// addTokens accumulates a pre-estimated input/output token pair for an account.
func (s *Server) addTokens(id string, in, out int64) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountTokenIn[id] += in
	s.accountTokenOut[id] += out
	s.statsDirty = true
}

// statsFile is the on-disk representation of usage counters so they survive a
// process/container restart.
type statsFile struct {
	Version  int              `json:"version"`
	Stats    map[string]int64 `json:"stats"`
	TokenIn  map[string]int64 `json:"tokenIn"`
	TokenOut map[string]int64 `json:"tokenOut"`
}

// loadStats restores the per-account counters from disk into memory.
func (s *Server) loadStats() error {
	if s.statsPath == "" {
		return nil
	}
	b, err := os.ReadFile(s.statsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f statsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Stats != nil {
		for k, v := range f.Stats {
			s.accountStats[k] = v
		}
	}
	if f.TokenIn != nil {
		for k, v := range f.TokenIn {
			s.accountTokenIn[k] = v
		}
	}
	if f.TokenOut != nil {
		for k, v := range f.TokenOut {
			s.accountTokenOut[k] = v
		}
	}
	return nil
}

// saveStats writes the current counters to disk atomically.
func (s *Server) saveStats() error {
	if s.statsPath == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.statsDirty {
		return nil
	}
	f := statsFile{
		Version:  1,
		Stats:    make(map[string]int64, len(s.accountStats)),
		TokenIn:  make(map[string]int64, len(s.accountTokenIn)),
		TokenOut: make(map[string]int64, len(s.accountTokenOut)),
	}
	for k, v := range s.accountStats {
		f.Stats[k] = v
	}
	for k, v := range s.accountTokenIn {
		f.TokenIn[k] = v
	}
	for k, v := range s.accountTokenOut {
		f.TokenOut[k] = v
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteFile(s.statsPath, b, 0o600); err != nil {
		return err
	}
	s.statsDirty = false
	return nil
}

// statsSaver periodically flushes counters to disk so a restart does not lose
// more than a few seconds of usage data.
func (s *Server) statsSaver() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		dirty := s.statsDirty
		s.mu.Unlock()
		if dirty {
			if err := s.saveStats(); err != nil {
				log.Printf("[stats] persist: %v", err)
			}
		}
	}
}

// resetStats zeroes all counters (totals and per-account) and flushes to disk.
func (s *Server) resetStats() error {
	s.mu.Lock()
	s.accountStats = make(map[string]int64)
	s.accountTokenIn = make(map[string]int64)
	s.accountTokenOut = make(map[string]int64)
	s.statsDirty = true
	s.mu.Unlock()
	return s.saveStats()
}

// resetStatsHandler handles POST /api/admin/reset-stats.
func (s *Server) resetStatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator session required")
		return
	}
	if err := s.resetStats(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "storage_error", "could not persist statistics reset")
		return
	}
	jsonOut(w, map[string]any{"status": "reset", "totalRequestCount": 0, "totalTokenIn": 0, "totalTokenOut": 0})
}

// Close flushes pending usage counters during graceful shutdown.
func (s *Server) Close() error {
	return s.saveStats()
}

func (s *Server) refreshAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	requestedID := strings.TrimSpace(body.ID)
	acc, err := s.refreshActiveAccount(requestedID)
	if err != nil {
		if errors.Is(err, errInactiveAccount) {
			writeOpenAIError(w, http.StatusConflict, "account_isolated", "only the active account may refresh its token")
			return
		}
		writeOpenAIError(w, http.StatusBadGateway, "token_refresh_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "refreshed", "account": map[string]any{
		"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName,
		"status": acc.Status, "expiresAt": acc.ExpiresAt, "updatedAt": acc.UpdatedAt,
	}})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := s.deleteAccountToken(body.ID); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	delete(s.accountStats, body.ID)
	delete(s.accountTokenIn, body.ID)
	delete(s.accountTokenOut, body.ID)
	s.statsDirty = true
	s.mu.Unlock()
	removed, err := s.sessions.deleteByAccount(body.ID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "storage_error", "account deleted but session cleanup could not be persisted")
		return
	}
	jsonOut(w, map[string]any{"status": "deleted", "sessionsRemoved": removed})
}

func (s *Server) updateAccountProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID    string `json:"id"`
		Proxy string `json:"proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	body.Proxy = strings.TrimSpace(body.Proxy)
	cfg, err := proxy.Parse(body.Proxy)
	if err != nil {
		http.Error(w, "invalid proxy format: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.tokens.SetProxy(body.ID, cfg.Raw); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	jsonOut(w, map[string]any{"status": "ok", "proxy": cfg.Raw})
}

func (s *Server) testProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Proxy string `json:"proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	jsonOut(w, egressProbeRunner(r.Context(), body.Proxy, egressProbeURL))
}

// testAllProxies probes every account, including direct accounts, with the same
// end-to-end metric as testProxy. Accounts sharing one canonical fixed egress
// reuse one probe result. Results always retain persisted account order.
func (s *Server) testAllProxies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	list := s.tokens.List()
	type result struct {
		AccountID    string `json:"accountId"`
		Email        string `json:"email"`
		DisplayName  string `json:"displayName,omitempty"`
		Proxy        string `json:"proxy"`
		OK           bool   `json:"ok"`
		Direct       bool   `json:"direct,omitempty"`
		IP           string `json:"ip,omitempty"`
		Status       int    `json:"status,omitempty"`
		LatencyMs    int64  `json:"latencyMs"`
		EgressHTTPMs int64  `json:"egressHttpMs"`
		Error        string `json:"error,omitempty"`
	}
	results := make([]result, len(list))
	proxyGroups := make(map[string][]int)
	groupProxy := make(map[string]string)
	for i, a := range list {
		rawProxy := strings.TrimSpace(a.Proxy)
		results[i] = result{
			AccountID:   a.ID,
			Email:       a.Email,
			DisplayName: a.DisplayName,
			Proxy:       rawProxy,
		}
		cfg, err := proxy.Parse(rawProxy)
		if err != nil {
			results[i].Error = "invalid proxy configuration"
			continue
		}
		identity := canonicalProxyIdentity(cfg)
		proxyGroups[identity] = append(proxyGroups[identity], i)
		groupProxy[identity] = rawProxy
	}

	var wg sync.WaitGroup
	workers := make(chan struct{}, maxEgressProbeWorkers)
	for identity, resultIndexes := range proxyGroups {
		rawProxy := groupProxy[identity]
		resultIndexes := append([]int(nil), resultIndexes...)
		wg.Add(1)
		go func() {
			defer wg.Done()
			workers <- struct{}{}
			probe := egressProbeRunner(r.Context(), rawProxy, egressProbeURL)
			<-workers
			for _, resultIndex := range resultIndexes {
				results[resultIndex].OK = probe.OK
				results[resultIndex].Direct = probe.Direct
				results[resultIndex].IP = probe.IP
				results[resultIndex].Status = probe.Status
				results[resultIndex].LatencyMs = probe.LatencyMs
				results[resultIndex].EgressHTTPMs = probe.EgressHTTPMs
				results[resultIndex].Error = probe.Error
			}
		}()
	}
	wg.Wait()
	okCount := 0
	for _, r := range results {
		if r.OK {
			okCount++
		}
	}
	jsonOut(w, map[string]any{
		"results":      results,
		"total":        len(results),
		"uniqueEgress": len(proxyGroups),
		"ok":           okCount,
		"failed":       len(results) - okCount,
	})
}

func (s *Server) startPKCE(w http.ResponseWriter, _ *http.Request) {
	v, err := auth.Verifier()
	if err != nil {
		http.Error(w, "pkce failure", http.StatusInternalServerError)
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "state failure", http.StatusInternalServerError)
		return
	}
	state := hex.EncodeToString(b)
	s.mu.Lock()
	s.pkce[state] = pendingPKCE{Verifier: v, Created: time.Now()}
	s.mu.Unlock()
	jsonOut(w, map[string]string{
		"status": "pkce_ready",
		"state":  state,
		"url": auth.AuthorizationURL(
			auth.AuthorizeEndpoint(),
			auth.ClientID(),
			auth.RedirectURI(),
			state,
			auth.Challenge(v),
			auth.Scope(),
		),
		"redirectUri": auth.RedirectURI(),
		"note":        "If redirect is nativeclient, paste the final URL/code into /api/auth/callback after login.",
	})
}

func (s *Server) callbackPKCE(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	// also accept pasted full callback URL
	if code == "" {
		if u := r.URL.Query().Get("url"); u != "" {
			if parsed, err := http.NewRequest(http.MethodGet, u, nil); err == nil {
				code = parsed.URL.Query().Get("code")
				if state == "" {
					state = parsed.URL.Query().Get("state")
				}
			}
		}
	}
	if state == "" || code == "" {
		http.Error(w, "missing state or code", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	p, ok := s.pkce[state]
	if ok {
		delete(s.pkce, state)
	}
	s.mu.Unlock()
	if !ok || time.Since(p.Created) > 10*time.Minute {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}
	tok, err := auth.ExchangeCode(code, p.Verifier, auth.RedirectURI())
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	acc, err := s.tokens.Upsert(tok)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := s.clearAuthFailureAfterCredentialUpdate(acc.ID); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := s.syncActiveAccount(); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	jsonOut(w, map[string]any{
		"status":  "authenticated",
		"account": map[string]any{"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName, "status": acc.Status, "oid": acc.OID, "tid": acc.TID},
	})
}

// stickyAccountsEnabled is retained for compatibility with existing policy
// tests and configuration introspection. Production routing is always
// single-active-account; an environment variable can no longer re-enable
// per-request round robin.
func stickyAccountsEnabled() bool {
	return true
}

func (s *Server) resolveAccount(accountID string) (auth.AccountToken, error) {
	return s.resolveActiveAccount()
}

var errPinnedAccountUnavailable = errors.New("pinned account temporarily unavailable")

// resolveRequestAccount always begins with the one global active identity.
// Session bindings are metadata only: when they refer to an older account the
// caller rebuilds on the active account. Explicit accountId may confirm the
// active identity but can never wake an isolated account.
func (s *Server) resolveRequestAccount(accountID string, explicit bool) (auth.AccountToken, bool, error) {
	requestedID := strings.TrimSpace(accountID)
	acc, err := s.resolveAccount("")
	preflightSwitched := false
	if err != nil {
		// resolveActiveAccount retired the failed active identity. Validate only
		// the new active identity without allowing this request to consume a
		// second routing slot; never scan the dormant pool.
		acc, err = s.resolveActiveAccountWithoutAdvance()
		if err != nil {
			return auth.AccountToken{}, false, err
		}
		preflightSwitched = true
	}
	if requestedID != "" && requestedID != acc.ID {
		if explicit {
			return auth.AccountToken{}, false, errInactiveAccount
		}
		preflightSwitched = true
	}
	return acc, preflightSwitched, nil
}

func writeAccountResolutionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errUpstreamCircuitOpen):
		w.Header().Set("Retry-After", fmt.Sprint(int(upstreamCircuitRest.Seconds())))
		writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_temporarily_unavailable", "shared upstream connectivity is unstable; retry after the circuit rest window")
	case errors.Is(err, errInactiveAccount), errors.Is(err, errPinnedAccountUnavailable):
		writeOpenAIError(w, http.StatusConflict, "account_temporarily_unavailable", "the active account changed or is resting; retry the request")
	default:
		writeOpenAIError(w, http.StatusBadRequest, "account_error", "no usable active account is available")
	}
}

type chatBody struct {
	AccountID      string               `json:"accountId"`
	Message        string               `json:"message"`
	Prompt         string               `json:"prompt"`
	Tone           string               `json:"tone"`
	ConversationID string               `json:"conversationId"`
	SessionID      string               `json:"sessionId"`
	SessionKey     string               `json:"sessionKey"`
	Attachments    []chathub.Attachment `json:"attachments,omitempty"`
	Tools          []chathub.Tool       `json:"tools,omitempty"`
	// Legacy OpenAI-compatible clients still send functions/function_call.
	Functions       []json.RawMessage `json:"functions,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
	FunctionCall    any               `json:"function_call,omitempty"`
	Reasoning       *reasoningConfig  `json:"reasoning,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
}

func modelTone(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-5.2":
		return "Gpt_5_2_Chat"
	case "gpt-5.2-reasoning":
		return "Gpt_5_2_Reasoning"
	case "gpt-5.3":
		return "Gpt_5_3_Chat"
	case "gpt-5.4":
		return "Gpt_5_4_Chat"
	case "gpt-5.4-reasoning":
		return "Gpt_5_4_Reasoning"
	case "gpt-5.5":
		return "Gpt_5_5_Chat"
	case "gpt-5.5-reasoning":
		return "Gpt_5_5_Reasoning"
	case "gpt-5.6", "gpt-5.6-sol", "gpt-5.6-terra":
		return "Gpt_5_6_Chat"
	case "gpt-5.6-reasoning", "gpt-5.6-luna":
		return "Gpt_5_6_Reasoning"
	case "gpt-5.3-reasoning":
		return "Gpt_5_3_Reasoning"
	case "claude", "claude-sonnet":
		return "Claude_Sonnet"
	case "claude-sonnet-reasoning":
		return "Claude_Sonnet_Reasoning"
	case "gpt-5.4-quick":
		return "Gpt_5_4_Chat"
	case "gpt-5.3-think-deeper":
		return "Gpt_5_3_Chat"
	case "quick":
		return "Gpt_Quick"
	case "think-deeper":
		return "Gpt_Reasoning"
	default:
		return "magic"
	}
}

func (s *Server) chatOnce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body chatBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
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
		// try extract from access token claims on the fly
		if claimsOID, claimsTID := extractOIDTID(acc.AccessToken); claimsOID != "" {
			acc.OID = claimsOID
			acc.TID = claimsTID
		}
	}
	if acc.OID == "" || acc.TID == "" {
		http.Error(w, "account missing oid/tid; re-login with PKCE browser client", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	req := chathub.Request{
		Text:           text,
		Tone:           body.Tone,
		ConversationID: body.ConversationID,
		SessionID:      body.SessionID,
		Attachments:    body.Attachments,
	}
	account := chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID, Proxy: acc.Proxy}
	res, err := s.chatActive(ctx, acc.ID, account, req)
	switched := preflightSwitched
	markTerminalFailure := func(failure error) {
		if switched {
			s.recordAccountFailureWithoutAdvance(acc.ID, failure)
			return
		}
		s.markAccountResult(acc.ID, failure)
	}
	if r.Context().Err() == nil && shouldFailoverAccount(explicitAccountID != "", switched, false, err, res, body.Tools) {
		failure := err
		if failure == nil {
			failure = strictQuotaExhaustedError()
		}
		advanced := s.markAccountResult(acc.ID, failure)
		// Only the request that won the active-account CAS may replay on the
		// successor. Concurrent failures from the retired generation return to
		// their callers instead of stampeding the fresh account.
		if advanced {
			if next, nerr := s.nextHealthyAccount(acc.ID); nerr == nil {
				if next.OID == "" || next.TID == "" {
					next.OID, next.TID = extractOIDTID(next.AccessToken)
				}
				if next.OID != "" && next.TID != "" {
					acc = next
					activeAccountID = next.ID
					account = chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID, Proxy: next.Proxy}
					req.ConversationID = ""
					req.SessionID = ""
					switched = true
					res, err = s.chatActive(ctx, acc.ID, account, req)
				}
			}
		}
	}
	if err != nil {
		if r.Context().Err() == nil {
			markTerminalFailure(err)
		}
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	if quotaFailureWithoutContent(res, body.Tools) {
		markTerminalFailure(strictQuotaExhaustedError())
		writeOpenAIError(w, http.StatusBadGateway, "upstream_throttled", "Microsoft Copilot quota exhausted")
		return
	}
	s.markAccountSuccess(acc.ID, res.Throttling)
	s.recordTokens(acc.ID, text, res.FullText)
	if body.SessionKey != "" {
		if err := s.persistSession(body.SessionKey, acc.ID, text, res); err != nil {
			log.Printf("[sessions] persist chat session: %v", err)
			writeOpenAIError(w, http.StatusInternalServerError, "session_persistence_error", "response state could not be persisted")
			return
		}
	}
	jsonOut(w, map[string]any{
		"status":          "ok",
		"text":            res.Text,
		"conversationId":  res.ConversationID,
		"sessionId":       res.SessionID,
		"requestId":       res.RequestID,
		"throttling":      res.Throttling,
		"result":          res.RawResult,
		"events":          res.Events,
		"eventsTruncated": res.EventsTruncated,
		"images":          res.Images,
		"account":         map[string]any{"id": acc.ID, "email": acc.Email},
	})
}

func (s *Server) openaiModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := modelCatalog()
	created := time.Now().Unix()
	for _, model := range data {
		model["created"] = created
	}
	jsonOut(w, map[string]any{"object": "list", "data": data})
}

// openaiModelDetail serves GET /v1/models/{id}. OpenAI-compatible desktop
// clients probe individual model entries after listing the catalog; without
// this route those probes 404 and clients treat the model as unverifiable.
func (s *Server) openaiModelDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/v1/models/"))
	if id == "" {
		s.openaiModels(w, r)
		return
	}
	canonical, ok := canonicalGatewayModel(id)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "model "+id+" not found")
		return
	}
	created := time.Now().Unix()
	for _, model := range modelCatalog() {
		if model["id"] == canonical {
			model["created"] = created
			jsonOut(w, model)
			return
		}
	}
	writeOpenAIError(w, http.StatusNotFound, "not_found", "model "+canonical+" not found")
}

type oaiMsg struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []map[string]any `json:"tool_calls,omitempty"`
}

type oaiReq struct {
	Model    string   `json:"model"`
	Messages []oaiMsg `json:"messages"`
	Stream   bool     `json:"stream"`
	// Instructions stays separate from portable Messages so a
	// previous_response_id never inherits stale developer policy.
	Instructions string `json:"_m365_instructions,omitempty"`
	// optional account routing
	User                           string               `json:"user"`
	AccountID                      string               `json:"accountId"`
	ConversationID                 string               `json:"conversation_id"`
	SessionID                      string               `json:"session_id"`
	SessionKey                     string               `json:"session_key"`
	SessionWriteKey                string               `json:"_m365_session_write_key,omitempty"`
	AllowResponsesToolContinuation bool                 `json:"_m365_responses_tool_continuation,omitempty"`
	RestorePortableHistory         bool                 `json:"_m365_restore_portable_history,omitempty"`
	Attachments                    []chathub.Attachment `json:"attachments,omitempty"`
	Tools                          []chathub.Tool       `json:"tools,omitempty"`
	// Legacy OpenAI-compatible clients still send functions/function_call.
	Functions       []json.RawMessage `json:"functions,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
	FunctionCall    any               `json:"function_call,omitempty"`
	Reasoning       *reasoningConfig  `json:"reasoning,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	// Responses API semantic controls are carried through the common request
	// so Hermes, OpenCode, and Codex share one tool orchestration path.
	ParallelToolCalls   *bool `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens     *int  `json:"max_output_tokens,omitempty"`
	MaxTokens           *int  `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int  `json:"max_completion_tokens,omitempty"`
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func contentToString(c any) string {
	if c == nil {
		return ""
	}
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if t, _ := m["type"].(string); t == "text" || t == "input_text" || t == "output_text" {
					if s, _ := m["text"].(string); s != "" {
						b.WriteString(s)
					}
				}
			}
		}
		return b.String()
	default:
		return fmt.Sprint(v)
	}
}

func normalizeLegacyTools(body *oaiReq) {
	if len(body.Tools) == 0 && len(body.Functions) > 0 {
		body.Tools = make([]chathub.Tool, 0, len(body.Functions))
		for _, f := range body.Functions {
			body.Tools = append(body.Tools, chathub.Tool{Type: "function", Function: f})
		}
	}
	if body.ToolChoice == nil && body.FunctionCall != nil {
		body.ToolChoice = body.FunctionCall
	}
}

// sseRaw writes one SSE frame and flushes; a write error (client gone,
// deadline exceeded) or a canceled request context aborts the stream
// instead of blocking a goroutine against a dead/slow socket.
func sseRaw(ctx context.Context, w http.ResponseWriter, f http.Flusher, payload string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(w, payload); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// streamAnswerRule / syncAnswerRule mirror the two FINAL ANSWER RULE suffixes
// used by the streaming and synchronous answer prompts. They are shared with
// rebuildFullHistoryPrompt so a failover-rebuilt prompt keeps the same shape.
const (
	streamAnswerRule = "FINAL ANSWER RULE: Answer the user directly. If a tool is explicitly required, emit its structured call; otherwise return ordinary text."
	syncAnswerRule   = "FINAL ANSWER RULE: Report only actions supported by completed tool results. If the goal is not fully verified, state exactly what remains unconfirmed."
)

// rebuildFullHistoryPrompt recomposes an answer prompt from a full recent-
// history tail instead of the continuing trim. Account failover starts a fresh
// upstream conversation (ConversationID cleared); that conversation has no
// memory of prior turns, so resending only the current turn would leave the
// model without any working context mid-thread.
func rebuildFullHistoryPrompt(messages []oaiMsg, model string, tools []chathub.Tool, attachments []chathub.Attachment, routerContext, rule string) (string, bool) {
	msgs, stats := selectPromptMessages(messages, model, tools, attachments, false)
	if stats.Exceeded || len(msgs) == 0 {
		return "", false
	}
	p, _ := flattenPromptMessages(msgs, attachments)
	p = strings.TrimSpace(p)
	if p == "" {
		return "", false
	}
	return p + "\n" + routerContext + "\n" + rule, true
}

// conversationRotateThreshold is how close to the upstream 600-messages-per-
// conversation cap a thread may get before the bridge forces a fresh upstream
// conversation. History backfill (selectPromptMessages) keeps continuity, so
// rotation is invisible to the client.
const conversationRotateThreshold = 550

// outputSoftCapTokens approximates the upstream's ~3k-token soft output cap:
// long generations conclude early instead of truncating, so a response at or
// above this size is likely incomplete even though it reads as finished.
const outputSoftCapTokens = 3000

func requestedOutputLimit(body oaiReq, configured int) (int, error) {
	limit := configured
	for _, candidate := range []*int{body.MaxOutputTokens, body.MaxCompletionTokens, body.MaxTokens} {
		if candidate == nil {
			continue
		}
		if *candidate <= 0 {
			return 0, fmt.Errorf("output token limit must be greater than zero")
		}
		if limit <= 0 || *candidate < limit {
			limit = *candidate
		}
	}
	return limit, nil
}

// truncateToOutputLimit applies the visible-output budget with the same local
// tokenizer used for usage accounting. ChatHub does not expose hidden reasoning
// tokens, so this is intentionally a visible-output limit.
func truncateToOutputLimit(model, text string, limit int) (string, bool) {
	if limit <= 0 || countTokens(model, text) <= int64(limit) {
		return text, false
	}
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		if countTokens(model, string(runes[:mid])) <= int64(limit) {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return string(runes[:low]), true
}

// shouldRotateConversation reports whether the upstream throttling payload
// shows the conversation is close to its message cap.
func shouldRotateConversation(throttling any) bool {
	m, ok := throttling.(map[string]any)
	if !ok {
		return false
	}
	n, ok := m["numUserMessagesInConversation"].(float64)
	return ok && n >= conversationRotateThreshold
}

// persistSession records the thread's upstream binding. When the upstream
// conversation is nearing its 600-message cap the binding is cleared instead,
// forcing the next turn onto a fresh conversation; history backfill preserves
// working context across the rotation.
func (s *Server) persistSession(sessionKey, accountID, prompt string, res chathub.Result, clearUpstream ...bool) error {
	if sessionKey == "" {
		return nil
	}
	convID, sessID := res.ConversationID, res.SessionID
	clear := len(clearUpstream) > 0 && clearUpstream[0]
	if !clear && convID == "" && sessID == "" {
		if existing, ok := s.sessions.get(sessionKey); ok && existing.AccountID == accountID {
			convID, sessID = existing.ConversationID, existing.SessionID
		}
	}
	if shouldRotateConversation(res.Throttling) {
		log.Printf("[sessions] rotating conversation near message cap for %s", sessionKey)
		convID, sessID = "", ""
	}
	generation, authoritative := s.accountRouteVersion(accountID)
	_, applied, err := s.sessions.upsertBinding(conversation{ID: sessionKey, AccountID: accountID, ConversationID: convID, SessionID: sessID, Title: boundedSessionTitle(prompt), RouteGeneration: generation}, authoritative)
	if err != nil {
		log.Printf("[sessions] persist session: %v", err)
		return err
	}
	if !applied {
		log.Printf("[sessions] ignored stale binding write for %s account=%s generation=%d", sessionKey, accountID, generation)
	}
	return nil
}

func (s *Server) openaiChat(w http.ResponseWriter, r *http.Request) {
	// sseSafe serializes all client SSE writes for this request so the
	// keepalive ticker and delta emission never interleave bytes.
	var sseMu sync.Mutex
	sseSafe := func(ctx context.Context, w http.ResponseWriter, f http.Flusher, payload string) error {
		sseMu.Lock()
		defer sseMu.Unlock()
		return sseRaw(ctx, w, f, payload)
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var body oaiReq
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	configuredOutputLimit := defaultRuntimeSettings().MaxOutputTokens
	if s.settings != nil {
		configuredOutputLimit = s.settings.get().MaxOutputTokens
	}
	outputLimit, err := requestedOutputLimit(body, configuredOutputLimit)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	canonicalModel, supported := canonicalGatewayModel(body.Model)
	if !supported {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "unsupported model "+body.Model)
		return
	}
	body.Model = canonicalModel
	effort := body.ReasoningEffort
	if body.Reasoning != nil && strings.TrimSpace(body.Reasoning.Effort) != "" {
		effort = body.Reasoning.Effort
	}
	tone, toneErr := reasoningTone(body.Model, effort)
	if toneErr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", toneErr.Error())
		return
	}
	toolRouterTone := modelToolRouterTone(body.Model)
	normalizeLegacyTools(&body)
	explicitAccountID := strings.TrimSpace(body.AccountID)
	activeAccountID := explicitAccountID
	explicitAccount := explicitAccountID != ""
	accountSwitched := false
	body.SessionKey = scopedSessionKey(r, body.SessionKey)
	body.SessionWriteKey = scopedSessionKey(r, body.SessionWriteKey)
	persistKey := firstNonEmpty(body.SessionWriteKey, body.SessionKey)
	continuingSession := false
	portableRestored := false
	if body.SessionKey != "" {
		if v, ok := s.sessions.get(body.SessionKey); ok {
			if body.RestorePortableHistory && len(v.PortableMessages) > 0 {
				body.Messages = mergePortableMessages(v.PortableMessages, body.Messages)
				portableRestored = true
			}
			if activeAccountID == "" {
				activeAccountID = v.AccountID
			}
			// Upstream conversation/session IDs are account-owned. Load them only
			// when the effective account matches the stored binding; an explicit
			// override to another account must start a fresh bounded-history chat.
			if activeAccountID != "" && activeAccountID == v.AccountID {
				body.ConversationID = firstNonEmpty(body.ConversationID, v.ConversationID)
				body.SessionID = firstNonEmpty(body.SessionID, v.SessionID)
				continuingSession = body.ConversationID != "" && body.SessionID != ""
			}
			// A legacy alias may predate portable history. It remains usable while
			// its account is still globally active, but it must never reactivate an
			// isolated account merely to resolve a tool result.
			if body.RestorePortableHistory && !portableRestored && allowResponsesToolContinuation(body) {
				currentID := s.currentActiveAccountID()
				if v.AccountID != "" && currentID != "" && v.AccountID != currentID {
					writeOpenAIError(w, http.StatusConflict, "previous_response_unavailable", "previous_response_id predates portable history and cannot be migrated to the active account")
					return
				}
				// A tool result is authoritative only when the referenced response
				// contains the matching assistant function call. Legacy aliases have
				// no such evidence, so fail locally instead of accepting an orphan
				// output and sending a corrupted turn upstream.
				writeOpenAIError(w, http.StatusConflict, "previous_response_unavailable", "previous_response_id predates verifiable tool history; start a new response")
				return
			}
		} else if body.RestorePortableHistory {
			// previous_response_id is a reference, not a hint to start a new
			// conversation. Accepting an unknown or expired alias would send an
			// orphan function_call_output (or context-free continuation) to the
			// active Microsoft account, which wastes quota and can make agent
			// clients loop indefinitely. Fail locally without touching upstream.
			writeOpenAIError(w, http.StatusConflict, "previous_response_unavailable", "previous_response_id is unknown, expired, or belongs to another API credential")
			return
		}
	}
	body.Messages = withRequestInstructions(body.Messages, body.Instructions)
	if err := validateToolConversation(body.Messages); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "tool_protocol_error", err.Error())
		return
	}
	// Rebuild a protocol-neutral evidence ledger from the restored transcript,
	// including the original assistant function call before its tool result.
	activeLedger := buildAgentLedger(activeMessages(body.Messages))
	if err := activeLedger.CanContinue(maxToolRounds()); err != nil {
		errorType := "tool_round_limit"
		if activeLedger.RepeatedFailure {
			errorType = "tool_loop_detected"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": errorType, "message": err.Error(), "completed_calls": len(activeLedger.Completed)}})
		return
	}
	// Preserve role boundaries while enforcing a real global input budget. A
	// persisted upstream conversation only needs the current turn plus current
	// instructions; a new conversation receives a bounded recent-history tail.
	originalAttachments := append([]chathub.Attachment(nil), body.Attachments...)
	promptMessages, promptStats := selectPromptMessages(body.Messages, body.Model, body.Tools, originalAttachments, continuingSession)
	if promptStats.Exceeded {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "context_length_exceeded", promptStats.ExceededReason)
		return
	}
	prompt, selectedAttachments := flattenPromptMessages(promptMessages, body.Attachments)
	body.Attachments = selectedAttachments
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		http.Error(w, "messages required", http.StatusBadRequest)
		return
	}
	if promptStats.DroppedMessages > 0 {
		log.Printf("[context] trimmed messages=%d->%d prompt_tokens=%d budget=%d tool_tokens=%d attachment_tokens=%d task_anchor_messages=%d task_anchor_tokens=%d continuing=%t", promptStats.OriginalMessages, promptStats.SelectedMessages, promptStats.PromptTokens, promptStats.PromptBudget, promptStats.ToolTokens, promptStats.AttachmentTokens, promptStats.AnchoredMessages, promptStats.TaskAnchorTokens, promptStats.Continuing)
	}
	// A policy-refused prompt must never be sprayed across the dormant account
	// pool by an automatic Hermes/OpenCode retry. The refusing identity is
	// rested for account safety, while this exact effective prompt is rejected
	// locally until the refusal window expires. A different task may advance in
	// the configured deterministic account order.
	if s.recentPolicyRefusal(prompt) {
		writeOpenAIError(w, http.StatusUnprocessableEntity, "policy_refusal", "this exact prompt was recently refused by upstream policy and will not be replayed under another account")
		return
	}
	acc, preflightSwitched, err := s.resolveRequestAccount(activeAccountID, explicitAccount)
	if err != nil {
		if errors.Is(err, errUpstreamCircuitOpen) {
			writeAccountResolutionError(w, err)
		} else {
			http.Error(w, "bad request", http.StatusBadRequest)
		}
		return
	}
	if preflightSwitched {
		accountSwitched = true
		activeAccountID = acc.ID
		body.ConversationID = ""
		body.SessionID = ""
		continuingSession = false
		promptMessages, promptStats = selectPromptMessages(body.Messages, body.Model, body.Tools, originalAttachments, false)
		if promptStats.Exceeded {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "context_length_exceeded", promptStats.ExceededReason)
			return
		}
		prompt, selectedAttachments = flattenPromptMessages(promptMessages, originalAttachments)
		body.Attachments = selectedAttachments
		prompt = strings.TrimSpace(prompt)
		if prompt == "" {
			http.Error(w, "messages required", http.StatusBadRequest)
			return
		}
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

	// Normalize tools once. Selection is always made by the upstream model;
	// the gateway only validates its structured decision and converts protocols.
	toolMaps := make([]map[string]any, 0, len(body.Tools))
	for _, tool := range body.Tools {
		var f map[string]any
		_ = json.Unmarshal(tool.Function, &f)
		toolMaps = append(toolMaps, map[string]any{"type": tool.Type, "function": f})
	}
	if body.ToolChoice == nil && len(toolMaps) > 0 {
		body.ToolChoice = "auto"
	}
	routerInput := modelToolRouterContext(promptMessages, prompt, activeLedger)
	routerInput += activeLedger.RecoveryInstruction()
	routerInput += fmt.Sprintf("\n[TOOL_CALL_LIMIT]\nReturn no more than %d call(s) in this turn.", effectiveToolCallLimit(body.ParallelToolCalls, s.settings))
	filterRepeatedCalls := func(calls []detectedToolCall) []detectedToolCall {
		filtered, removed := removeRepeatedToolCalls(calls, activeLedger)
		if removed > 0 {
			log.Printf("[tool-loop] suppressed=%d reason=identical_call_without_progress", removed)
		}
		return filtered
	}

	baseCtx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	ctx, releaseRequestLease, leaseErr := s.beginActiveAccountRequest(baseCtx, acc.ID)
	if leaseErr != nil {
		writeOpenAIError(w, http.StatusConflict, "account_route_changed", "active account changed while the request was starting; retry the request")
		return
	}
	defer func() { releaseRequestLease() }()
	account := chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID, Proxy: acc.Proxy}
	activateNextAccount := func(failure error, result chathub.Result, visibleOutput bool) bool {
		if ctx.Err() != nil || r.Context().Err() != nil {
			return false
		}
		if !shouldFailoverAccount(explicitAccount, accountSwitched, visibleOutput, failure, result, body.Tools) {
			return false
		}
		if failure == nil {
			failure = strictQuotaExhaustedError()
		}
		if !s.markAccountResult(acc.ID, failure) {
			return false
		}
		next, nextErr := s.nextHealthyAccount(acc.ID)
		if nextErr != nil {
			return false
		}
		if next.OID == "" || next.TID == "" {
			if oid, tid := extractOIDTID(next.AccessToken); oid != "" {
				next.OID, next.TID = oid, tid
			}
		}
		if next.OID == "" || next.TID == "" {
			return false
		}
		// The logical turn, not each individual ChatHub subcall, owns the
		// identity lease. Move that lease exactly once together with the
		// sequential failover decision.
		releaseRequestLease()
		nextCtx, nextRelease, leaseErr := s.beginActiveAccountRequest(baseCtx, next.ID)
		if leaseErr != nil {
			return false
		}
		ctx = nextCtx
		releaseRequestLease = nextRelease
		acc = next
		activeAccountID = next.ID
		account = chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID, Proxy: next.Proxy}
		body.ConversationID = ""
		body.SessionID = ""
		continuingSession = false
		accountSwitched = true
		return true
	}
	markTerminalFailure := func(failure error) {
		if IsDisengaged(failure) {
			s.rememberPolicyRefusal(prompt)
		}
		// The request deadline/cancellation belongs to this logical operation and
		// must not quarantine an otherwise healthy identity.
		if ctx.Err() != nil || r.Context().Err() != nil {
			return
		}
		if accountSwitched {
			s.recordAccountFailureWithoutAdvance(acc.ID, failure)
			return
		}
		s.markAccountResult(acc.ID, failure)
	}
	var streamFlusher http.Flusher
	stopKeepAlive := func() {}
	if body.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		var ok bool
		streamFlusher, ok = w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		// Keep the client (and any edge proxy such as Cloudflare) alive during
		// slow upstream generation or ChatHub reconnects. Without this, a long
		// silent reasoning stretch or a stalled WebSocket read looks like a dead
		// connection and the edge kills it (524 / client timeout) even though
		// the bridge would otherwise recover on its own.
		keepAliveDone := make(chan struct{})
		var keepAliveOnce sync.Once
		var keepAliveWG sync.WaitGroup
		keepAliveWG.Add(1)
		go func() {
			defer keepAliveWG.Done()
			tk := time.NewTicker(15 * time.Second)
			defer tk.Stop()
			for {
				select {
				case <-keepAliveDone:
					return
				case <-tk.C:
					_ = sseSafe(r.Context(), w, streamFlusher, ": keepalive\n\n")
				}
			}
		}()
		stopKeepAlive = func() {
			keepAliveOnce.Do(func() { close(keepAliveDone) })
			keepAliveWG.Wait()
		}
		defer stopKeepAlive()
		// This transport-only frame deliberately does not count as model output.
		// It commits SSE headers immediately and allows the middleware heartbeat
		// to keep a slow router/model call alive while account failover remains safe.
		if err := sseSafe(r.Context(), w, streamFlusher, ": connected\n\n"); err != nil {
			return
		}
	}
	streamPrimed := false
	// Streaming requests must not wait for the synchronous tool router before
	// answering, but the router itself stays mandatory for tool turns: the
	// upstream model frequently answers tool requests in prose without ever
	// emitting a structured call event, so skipping the router lost tool calls
	// entirely. The router runs first; when it selects no tool the streamed
	// answer below forwards upstream text deltas immediately.
	if body.Stream && len(toolMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		// Keep one absolute budget for the whole routing phase, but derive every
		// individual call from the current account lease. A failover cancels the
		// old lease and replaces ctx; reusing a child of the old ctx made the next
		// SOCKS/WebSocket dial fail immediately with "operation was canceled".
		routerDeadline := time.Now().Add(toolRouterPhaseTimeout)
		chatRouter := func(request chathub.Request) (chathub.Result, error) {
			routerCtx, stopRouter := context.WithDeadline(ctx, routerDeadline)
			defer stopRouter()
			return s.chatActive(routerCtx, acc.ID, account, request)
		}
		// Preserve the existing validated tool router for streaming tool turns.
		// Only fall through to text streaming when the router explicitly selects
		// no tool; this prevents a natural-language preamble from becoming a
		// completed assistant turn with the actual call lost.
		// Router prompt is truncated to the current turn only. The router
		// doesn't need conversation history — it just picks the next tool.
		// Full-history prompts slow inference and waste upstream tokens.
		routePrompt := modelToolRouterPrompt(routerInput, toolMaps, body.ToolChoice)
		routeRes, routeErr := chatRouter(chathub.Request{Text: routePrompt, Tone: toolRouterTone})
		if os.Getenv("M365_ROUTER_DEBUG") != "" {
			log.Printf("[router-debug] stream tone=%s model=%s err=%v text=%.600s", toolRouterTone, body.Model, routeErr, routeRes.Text)
			log.Printf("[router-debug] prompt=%.1200s", routePrompt)
		}
		if routeErr != nil && activateNextAccount(routeErr, chathub.Result{}, false) {
			routeRes, routeErr = chatRouter(chathub.Request{Text: routePrompt, Tone: toolRouterTone})
		}
		if routeErr != nil {
			markTerminalFailure(routeErr)
			_ = sseSafe(r.Context(), w, streamFlusher, "data: "+mustJSON(map[string]any{"error": map[string]any{"message": "tool router: " + routeErr.Error(), "type": "upstream_error", "code": "upstream_error"}})+"\n\n")
			_ = sseSafe(r.Context(), w, streamFlusher, "data: [DONE]\n\n")
			return
		}
		calls, parsed := parseModelToolDecision(routeRes.Text, toolMaps, body.ToolChoice)
		if !parsed {
			repairReq := chathub.Request{Text: `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Use {"calls":[]} if no tool is needed. OUTPUT:\n` + compactToolResult(routeRes.Text, 6000), Tone: toolRouterTone}
			repairRes, repairErr := chatRouter(repairReq)
			if repairErr != nil && activateNextAccount(repairErr, chathub.Result{}, false) {
				repairRes, repairErr = chatRouter(repairReq)
			}
			if repairErr == nil {
				calls, parsed = parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
			}
		}
		if parsed {
			calls = filterRepeatedCalls(calls)
		}
		if (!parsed || len(calls) == 0) && toolChoiceRequiresCall(body.ToolChoice) {
			retryText := modelToolRequiredRetryPrompt(routerInput, toolMaps, body.ToolChoice)
			retryRes, retryErr := chatRouter(chathub.Request{Text: retryText, Tone: toolRouterTone})
			if retryErr != nil && activateNextAccount(retryErr, chathub.Result{}, false) {
				retryRes, retryErr = chatRouter(chathub.Request{Text: retryText, Tone: toolRouterTone})
			}
			if retryErr == nil {
				if retryCalls, retryParsed := parseModelToolDecision(retryRes.Text, toolMaps, body.ToolChoice); retryParsed && len(retryCalls) > 0 {
					calls, parsed, routeRes = filterRepeatedCalls(retryCalls), true, retryRes
				}
			}
		}
		if parsed && len(calls) > 0 {
			scope := fmt.Sprintf("%d:%v:stream", len(body.Messages), completedCallIDs(activeLedger))
			for i := range calls {
				calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
			}
			calls = limitToolCalls(calls, effectiveToolCallLimit(body.ParallelToolCalls, s.settings))
			// Deliberately NOT rebinding the session to the router's one-shot
			// conversation: the router runs in a throwaway ChatHub chat, and
			// rebinding wiped the thread's real upstream memory on every tool
			// call — agentic loops lost all continuity mid-task. The existing
			// session binding survives so the next answer turn continues the
			// thread's own conversation.
			// The router chat is throwaway, but Responses needs a durable source
			// record before it can alias the returned response ID. Preserve an
			// existing active-account conversation; after failover clear the old
			// account-owned IDs so the tool result is rebuilt from portable history.
			if err := s.persistSession(persistKey, activeAccountID, prompt, chathub.Result{}, accountSwitched); err != nil {
				_ = sseSafe(r.Context(), w, streamFlusher, "data: "+mustJSON(map[string]any{"error": map[string]any{"message": "response state could not be persisted", "type": "session_persistence_error", "code": "session_persistence_error"}})+"\n\n")
				_ = sseSafe(r.Context(), w, streamFlusher, "data: [DONE]\n\n")
				return
			}
			s.markAccountSuccess(acc.ID, routeRes.Throttling)
			stopKeepAlive()
			_ = writeToolResponseWithLimit(r.Context(), w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), true, calls, routeRes, outputLimit, streamPrimed)
			return
		}
		if toolChoiceRequiresCall(body.ToolChoice) {
			_ = sseSafe(r.Context(), w, streamFlusher, "data: "+mustJSON(map[string]any{"error": map[string]any{"message": "model did not select the required tool after constrained retry", "type": "upstream_error", "code": "required_tool_missing"}})+"\n\n")
			_ = sseSafe(r.Context(), w, streamFlusher, "data: [DONE]\n\n")
			return
		}
	}
	// Only append the tool-evidence answer rule when tools were offered. For
	// plain chat the rule poisons every response: the model reads "state
	// exactly what remains unconfirmed" and spends its output on disclaimers
	// instead of doing work.
	answerSuffix := ""
	if len(body.Tools) > 0 {
		answerSuffix = "\n" + activeLedger.RouterContext() + activeLedger.RecoveryInstruction() + "\n" + streamAnswerRule
	}
	if body.Stream {
		answerPrompt := prompt + answerSuffix
		if accountSwitched {
			rule := ""
			if len(body.Tools) > 0 {
				rule = streamAnswerRule
			}
			if rebuilt, ok := rebuildFullHistoryPrompt(body.Messages, body.Model, body.Tools, body.Attachments, activeLedger.RouterContext(), rule); ok {
				answerPrompt = rebuilt
			}
		}
		answerReq := chathub.Request{Text: answerPrompt, Tone: tone, ConversationID: body.ConversationID, SessionID: body.SessionID, Attachments: body.Attachments, Tools: body.Tools, ToolChoice: body.ToolChoice}
		id := "chatcmpl-" + uuid.NewString()
		model := firstNonEmpty(body.Model, "m365-copilot")
		textStreamed := false
		downstreamWriteFailed := false
		outputLimitReached := false
		var deliveredText strings.Builder
		var streamedTools []detectedToolCall
		var placeholderGuard streamPlaceholderGuard
		// When tools are available, hold natural-language deltas until the full
		// answer can be checked against the current turn's tool evidence.  Once a
		// client has seen a false "deployed successfully" delta it cannot be
		// retracted.  Chat clients still receive transport keepalives, while the
		// Responses adapter emits typed response.in_progress heartbeats.
		guardToolAnswer := len(body.Tools) > 0
		emitDelta := func(content string) error {
			if outputLimitReached {
				return nil
			}
			if released, held := placeholderGuard.Feed(content); held {
				return nil
			} else {
				content = released
			}
			if content != "" && outputLimit > 0 {
				combined := deliveredText.String() + content
				if clipped, truncated := truncateToOutputLimit(body.Model, combined, outputLimit); truncated {
					content = strings.TrimPrefix(clipped, deliveredText.String())
					outputLimitReached = true
				}
			}
			delta := map[string]any{}
			if !textStreamed {
				delta["role"] = "assistant"
			}
			if content != "" {
				delta["content"] = content
				deliveredText.WriteString(content)
			}
			if guardToolAnswer {
				return nil
			}
			chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}}
			if err := sseSafe(r.Context(), w, streamFlusher, "data: "+mustJSON(chunk)+"\n\n"); err != nil {
				downstreamWriteFailed = true
				return err
			}
			textStreamed = true
			return nil
		}
		res2, err := s.chatActiveWithDelta(ctx, acc.ID, account, answerReq, emitDelta)
		if !downstreamWriteFailed && r.Context().Err() == nil && activateNextAccount(err, res2, textStreamed) {
			placeholderGuard.Reset()
			answerReq.ConversationID = ""
			answerReq.SessionID = ""
			rule := ""
			if len(body.Tools) > 0 {
				rule = streamAnswerRule
			}
			// A fresh account has no upstream memory. Rebuild from the bounded
			// full request history for both tool and plain-chat turns.
			if rebuilt, ok := rebuildFullHistoryPrompt(body.Messages, body.Model, body.Tools, body.Attachments, activeLedger.RouterContext(), rule); ok {
				answerReq.Text = rebuilt
			}
			res2, err = s.chatActiveWithDelta(ctx, acc.ID, account, answerReq, emitDelta)
		}
		// A dropped WebSocket after visible text cannot be replayed from the
		// beginning on another account: that duplicates prose and can duplicate
		// side effects. ChatHub now returns the generated conversation/session IDs
		// with a partial error, so make one bounded continuation attempt on a new
		// socket in the same upstream conversation and emit only the non-overlap.
		if err != nil && outputLimitReached && textStreamed {
			res2.Text = deliveredText.String()
			res2.FullText = res2.Text
			err = nil
		}
		if err != nil && textStreamed && !downstreamWriteFailed && r.Context().Err() == nil && len(body.Tools) == 0 &&
			IsTransientUpstream(err) && res2.ConversationID != "" && res2.SessionID != "" {
			partialText := deliveredText.String()
			resume, resumeErr := s.chatActive(ctx, acc.ID, account, chathub.Request{
				Text: visibleStreamResumePrompt(partialText), Tone: tone,
				ConversationID: res2.ConversationID, SessionID: res2.SessionID,
			})
			if resumeErr == nil && quotaFailureWithoutContent(resume) {
				resumeErr = strictQuotaExhaustedError()
			}
			if resumeErr == nil && strings.TrimSpace(resume.Text) != "" {
				continuation := trimContinuationOverlap(partialText, resume.Text)
				if continuation != "" {
					if writeErr := emitDelta(continuation); writeErr != nil {
						return
					}
				}
				res2.Text = deliveredText.String()
				res2.FullText = res2.Text
				res2.ConversationID, res2.SessionID = resume.ConversationID, resume.SessionID
				res2.Throttling, res2.RawResult = resume.Throttling, resume.RawResult
				res2.Events = append(res2.Events, resume.Events...)
				res2.EventsTruncated = res2.EventsTruncated || resume.EventsTruncated
				res2.Images = append(res2.Images, resume.Images...)
				err = nil
				log.Printf("[stream-recovery] resumed visible partial response on the same account conversation")
			} else {
				if resumeErr == nil {
					resumeErr = errors.New("upstream returned an empty stream recovery")
				}
				err = fmt.Errorf("%w; same-conversation stream recovery failed: %v", err, resumeErr)
			}
		}
		if err == nil && quotaFailureWithoutContent(res2, body.Tools) {
			markTerminalFailure(strictQuotaExhaustedError())
			_ = sseSafe(r.Context(), w, streamFlusher, "data: "+mustJSON(map[string]any{"error": map[string]any{"message": "Microsoft Copilot quota exhausted (throttled), please retry later or refresh accounts", "type": "upstream_error", "code": "upstream_throttled"}})+"\n\n")
			_ = sseSafe(r.Context(), w, streamFlusher, "data: [DONE]\n\n")
			return
		}
		if pending := placeholderGuard.Finish(false); pending != "" {
			if err := emitDelta(pending); err != nil {
				return
			}
		}
		streamContinuationIncomplete := false
		if err == nil && len(nativeToolCalls(res2.Events, body.Tools)) == 0 && !outputLimitReached && res2.ConversationID != "" {
			lastSegment := res2.FullText
			for i := 0; i < 4 && countTokens(body.Model, lastSegment) >= outputSoftCapTokens && !outputLimitReached; i++ {
				firstDelta := true
				var continuationGuard streamPlaceholderGuard
				continuationDelta := func(content string) error {
					if released, held := continuationGuard.Feed(content); held {
						return nil
					} else {
						content = released
					}
					if firstDelta && content != "" {
						firstDelta = false
						if err := emitDelta("\n"); err != nil {
							return err
						}
					}
					return emitDelta(content)
				}
				cont, continuationErr := s.chatActiveWithDelta(ctx, acc.ID, account, chathub.Request{
					Text: "继续，不要重复，从上次最后一句接着写完整。", Tone: tone,
					ConversationID: res2.ConversationID, SessionID: res2.SessionID,
				}, continuationDelta)
				placeholder := isUpstreamThrottlePlaceholderText(firstNonEmpty(cont.Text, cont.FullText))
				if continuationErr != nil || strings.TrimSpace(cont.Text) == "" || placeholder {
					streamContinuationIncomplete = true
					if continuationErr != nil || placeholder {
						if placeholder {
							continuationErr = strictQuotaExhaustedError()
						}
						s.recordAccountFailureWithoutAdvance(acc.ID, continuationErr)
						log.Printf("[continuation] streamed continuation failed after %d segments: %v", i, continuationErr)
					}
					break
				}
				if pending := continuationGuard.Finish(false); pending != "" {
					if err := continuationDelta(pending); err != nil {
						return
					}
				}
				res2.Text += "\n" + cont.Text
				res2.FullText += "\n" + cont.FullText
				res2.ConversationID, res2.SessionID = cont.ConversationID, cont.SessionID
				res2.Events = append(res2.Events, cont.Events...)
				res2.Images = append(res2.Images, cont.Images...)
				lastSegment = cont.FullText
				if i == 3 && countTokens(body.Model, lastSegment) >= outputSoftCapTokens {
					streamContinuationIncomplete = true
				}
			}
		}
		if err == nil {
			if outputLimit > 0 {
				res2.Text, _ = truncateToOutputLimit(body.Model, res2.Text, outputLimit)
				res2.FullText, _ = truncateToOutputLimit(body.Model, res2.FullText, outputLimit)
			}
			// Some ChatHub completions contain the final text only in the terminal
			// completion event and emit no incremental update events.  Do not send a
			// content-free stop chunk in that valid upstream shape; backfill the
			// complete text through the same output-budgeted delta path.
			if err := backfillTerminalStreamText(res2.Text, &deliveredText, guardToolAnswer, emitDelta); err != nil {
				return
			}
			streamedTools = limitToolCalls(nativeToolCalls(res2.Events, body.Tools), effectiveToolCallLimit(body.ParallelToolCalls, s.settings))
			if guardToolAnswer && len(streamedTools) == 0 && !completionEvidenceAllows(res2.Text, activeLedger) {
				const unverified = "I cannot confirm completion because no successful matching tool results were returned. No external action has been verified."
				verifiedText := unverified
				if outputLimit > 0 {
					var truncated bool
					verifiedText, truncated = truncateToOutputLimit(body.Model, verifiedText, outputLimit)
					outputLimitReached = outputLimitReached || truncated
				}
				res2.Text = verifiedText
				res2.FullText = verifiedText
				deliveredText.Reset()
				deliveredText.WriteString(verifiedText)
			}
			remainingOutputLimit := outputLimit
			if remainingOutputLimit > 0 {
				remainingOutputLimit -= int(countTokens(body.Model, deliveredText.String()))
				if remainingOutputLimit < 1 || !toolCallsFitOutputLimit(body.Model, streamedTools, remainingOutputLimit) {
					streamedTools = nil
					outputLimitReached = true
				}
			}
			if guardToolAnswer && deliveredText.Len() > 0 {
				delta := map[string]any{"role": "assistant", "content": deliveredText.String()}
				chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}}
				if err := sseSafe(r.Context(), w, streamFlusher, "data: "+mustJSON(chunk)+"\n\n"); err != nil {
					return
				}
				textStreamed = true
			}
			if !textStreamed && len(streamedTools) > 0 {
				chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}}
				if err := sseSafe(r.Context(), w, streamFlusher, "data: "+mustJSON(chunk)+"\n\n"); err != nil {
					return
				}
			}
			for i, tc := range streamedTools {
				chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Name, "arguments": string(tc.Arguments)}}}}, "finish_reason": nil}}}
				if err := sseSafe(r.Context(), w, streamFlusher, "data: "+mustJSON(chunk)+"\n\n"); err != nil {
					return
				}
			}
			finishReason := "stop"
			if len(streamedTools) > 0 {
				finishReason = "tool_calls"
			} else if outputLimitReached || streamContinuationIncomplete {
				// Upstream concludes long outputs early (~3k tokens) rather than
				// truncating, so big writes look complete but end mid-task.
				// Signal clients to ask for continuation instead of trusting it.
				finishReason = "length"
			}
			s.markAccountSuccess(acc.ID, res2.Throttling)
			in, out := estimateChatUsage(firstNonEmpty(body.Model, "m365-copilot"), promptMessages, body.Tools, res2.FullText)
			s.addTokens(acc.ID, in, out)
			if err := s.persistSession(persistKey, acc.ID, prompt, res2, outputLimitReached); err != nil {
				_ = sseSafe(r.Context(), w, streamFlusher, "data: "+mustJSON(map[string]any{"error": map[string]any{"message": "response state could not be persisted", "type": "session_persistence_error", "code": "session_persistence_error"}})+"\n\n")
				_ = sseSafe(r.Context(), w, streamFlusher, "data: [DONE]\n\n")
				return
			}
			terminal := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}}}
			if err := sseSafe(r.Context(), w, streamFlusher, "data: "+mustJSON(terminal)+"\n\n"); err != nil {
				return
			}
			if err := sseSafe(r.Context(), w, streamFlusher, "data: [DONE]\n\n"); err != nil {
				return
			}
		} else {
			if !downstreamWriteFailed && r.Context().Err() == nil {
				markTerminalFailure(err)
				_ = sseSafe(r.Context(), w, streamFlusher, "data: "+mustJSON(map[string]any{"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error", "code": "upstream_error"}})+"\n\n")
				_ = sseSafe(r.Context(), w, streamFlusher, "data: [DONE]\n\n")
			}
		}
		return
	}
	// Ask the upstream model to select and validate the next tool. The gateway
	// remains tool-agnostic; it only validates and serializes the decision.
	if len(toolMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		routerDeadline := time.Now().Add(toolRouterPhaseTimeout)
		chatRouter := func(request chathub.Request) (chathub.Result, error) {
			routerCtx, stopRouter := context.WithDeadline(ctx, routerDeadline)
			defer stopRouter()
			return s.chatActive(routerCtx, acc.ID, account, request)
		}
		routePrompt := modelToolRouterPrompt(routerInput, toolMaps, body.ToolChoice)
		routeRes, routeErr := chatRouter(chathub.Request{Text: routePrompt, Tone: toolRouterTone})
		if routeErr != nil && activateNextAccount(routeErr, chathub.Result{}, false) {
			routeRes, routeErr = chatRouter(chathub.Request{Text: routePrompt, Tone: toolRouterTone})
		}
		if routeErr != nil {
			http.Error(w, "tool router: "+routeErr.Error(), http.StatusBadGateway)
			return
		}
		calls, parsed := parseModelToolDecision(routeRes.Text, toolMaps, body.ToolChoice)
		if !parsed {
			repairReq := chathub.Request{Text: `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Do not invent calls; use {"calls":[]} if unrecoverable. OUTPUT:
` + compactToolResult(routeRes.Text, 6000), Tone: toolRouterTone}
			repairRes, repairErr := chatRouter(repairReq)
			if repairErr != nil && activateNextAccount(repairErr, chathub.Result{}, false) {
				repairRes, repairErr = chatRouter(repairReq)
			}
			if repairErr == nil {
				calls, parsed = parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
			}
			if !parsed {
				http.Error(w, "model returned an invalid tool routing decision", http.StatusBadGateway)
				return
			}
		}
		if parsed {
			calls = filterRepeatedCalls(calls)
		}
		if len(calls) > 0 {
			scope := fmt.Sprintf("%d:%v", len(body.Messages), completedCallIDs(activeLedger))
			for i := range calls {
				calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
			}
			calls = limitToolCalls(calls, effectiveToolCallLimit(body.ParallelToolCalls, s.settings))
			// Same as the streaming router path: never rebind the session to
			// the router's throwaway conversation; keep the thread's own
			// upstream memory intact across tool calls.
			if err := s.persistSession(persistKey, activeAccountID, prompt, chathub.Result{}, accountSwitched); err != nil {
				writeOpenAIError(w, http.StatusInternalServerError, "session_persistence_error", "response state could not be persisted")
				return
			}
			stopKeepAlive()
			_ = writeToolResponseWithLimit(r.Context(), w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), false, calls, routeRes, outputLimit, streamPrimed)
			return
		}
		if toolChoiceRequiresCall(body.ToolChoice) {
			retryText := modelToolRequiredRetryPrompt(routerInput, toolMaps, body.ToolChoice)
			retryRes, retryErr := chatRouter(chathub.Request{Text: retryText, Tone: toolRouterTone})
			if retryErr != nil && activateNextAccount(retryErr, chathub.Result{}, false) {
				retryRes, retryErr = chatRouter(chathub.Request{Text: retryText, Tone: toolRouterTone})
			}
			if retryErr == nil {
				calls, parsed = parseModelToolDecision(retryRes.Text, toolMaps, body.ToolChoice)
				if parsed {
					calls = filterRepeatedCalls(calls)
				}
				if parsed && len(calls) > 0 {
					scope := fmt.Sprintf("%d:%v:required-retry", len(body.Messages), completedCallIDs(activeLedger))
					for i := range calls {
						calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
					}
					calls = limitToolCalls(calls, effectiveToolCallLimit(body.ParallelToolCalls, s.settings))
					// The required-retry router is a one-shot conversation just like the
					// normal router. Never bind the user's thread to its temporary IDs.
					if err := s.persistSession(persistKey, activeAccountID, prompt, chathub.Result{}, accountSwitched); err != nil {
						writeOpenAIError(w, http.StatusInternalServerError, "session_persistence_error", "response state could not be persisted")
						return
					}
					stopKeepAlive()
					_ = writeToolResponseWithLimit(r.Context(), w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), false, calls, retryRes, outputLimit, streamPrimed)
					return
				}
			}
			http.Error(w, "model did not select a required tool after constrained retry", http.StatusBadGateway)
			return
		}
	}
	nonStreamSuffix := ""
	if len(body.Tools) > 0 {
		nonStreamSuffix = "\n" + activeLedger.RouterContext() + activeLedger.RecoveryInstruction() + "\n" + syncAnswerRule
	}
	answerPrompt := prompt + nonStreamSuffix
	if accountSwitched {
		rule := ""
		if len(body.Tools) > 0 {
			rule = syncAnswerRule
		}
		if rebuilt, ok := rebuildFullHistoryPrompt(body.Messages, body.Model, body.Tools, body.Attachments, activeLedger.RouterContext(), rule); ok {
			answerPrompt = rebuilt
		}
	}
	answerReq := chathub.Request{Text: answerPrompt, Tone: tone, ConversationID: body.ConversationID, SessionID: body.SessionID, Attachments: body.Attachments}
	var res chathub.Result
	res, err = s.chatActive(ctx, acc.ID, account, answerReq)
	if activateNextAccount(err, res, false) {
		failoverText := answerPrompt
		rule := ""
		if len(body.Tools) > 0 {
			rule = syncAnswerRule
		}
		if rebuilt, ok := rebuildFullHistoryPrompt(body.Messages, body.Model, body.Tools, body.Attachments, activeLedger.RouterContext(), rule); ok {
			failoverText = rebuilt
		}
		res, err = s.chatActive(ctx, acc.ID, account, chathub.Request{
			Text:        failoverText,
			Tone:        tone,
			Attachments: body.Attachments,
		})
	}
	// Auto-continue non-stream answers that the upstream capped at its
	// ~3k-token soft limit (finish_reason="length"). Re-enter the same
	// ChatHub conversation with a continuation prompt and append the tail
	// so the client receives a complete answer instead of a truncated one.
	continuationIncomplete := false
	var continuationFailure error
	if err == nil && countTokens(body.Model, res.FullText) >= outputSoftCapTokens && res.ConversationID != "" && (outputLimit <= 0 || countTokens(body.Model, res.FullText) < int64(outputLimit)) {
		for i := 0; i < 4; i++ {
			cont, cerr := s.chatActive(ctx, acc.ID, account, chathub.Request{
				Text:           "继续，不要重复，从上次最后一句接着写完整。",
				Tone:           tone,
				ConversationID: res.ConversationID,
				SessionID:      res.SessionID,
			})
			if cerr != nil {
				continuationFailure = cerr
				s.recordAccountFailureWithoutAdvance(acc.ID, cerr)
				log.Printf("[continuation] failed after %d successful segments: %v", i, cerr)
				break
			}
			if strings.TrimSpace(cont.Text) == "" {
				continuationFailure = errors.New("upstream returned an empty continuation")
				log.Printf("[continuation] failed after %d successful segments: empty continuation", i)
				break
			}
			res.Text += "\n" + cont.Text
			res.FullText += "\n" + cont.FullText
			if len(cont.Events) > 0 {
				res.Events = append(res.Events, cont.Events...)
			}
			if outputLimit > 0 && countTokens(body.Model, res.FullText) >= int64(outputLimit) {
				continuationIncomplete = true
				break
			}
			if countTokens(body.Model, cont.FullText) < outputSoftCapTokens {
				break
			}
			if i == 3 {
				continuationIncomplete = true
				log.Printf("[continuation] incomplete after reaching the four-segment safety limit")
			}
		}
	}
	if continuationFailure != nil {
		// The initial answer and any successful continuation segments are valid
		// visible output. Preserve them and signal length/incomplete instead of
		// discarding useful work as a 502.
		continuationIncomplete = true
	}
	if err != nil {
		markTerminalFailure(err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	if quotaFailureWithoutContent(res, body.Tools) {
		markTerminalFailure(strictQuotaExhaustedError())
		writeOpenAIError(w, http.StatusBadGateway, "upstream_throttled", "Microsoft Copilot quota exhausted")
		return
	}
	outputTruncated := false
	if outputLimit > 0 {
		res.Text, outputTruncated = truncateToOutputLimit(body.Model, res.Text, outputLimit)
		res.FullText, _ = truncateToOutputLimit(body.Model, res.FullText, outputLimit)
	}
	s.markAccountSuccess(acc.ID, res.Throttling)
	in, out := estimateChatUsage(firstNonEmpty(body.Model, "m365-copilot"), promptMessages, body.Tools, res.FullText)
	s.addTokens(acc.ID, in, out)
	if err := s.persistSession(persistKey, acc.ID, prompt, res, outputTruncated); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "session_persistence_error", "response state could not be persisted")
		return
	}
	model := body.Model
	if model == "" {
		model = "m365-copilot"
	}
	id := "chatcmpl-" + uuid.NewString()
	if calls := fencedToolCalls(res.Text, toolMaps, body.ToolChoice); len(calls) > 0 {
		calls = limitToolCalls(calls, effectiveToolCallLimit(body.ParallelToolCalls, s.settings))
		_ = writeToolResponseWithLimit(r.Context(), w, id, model, false, calls, res, outputLimit)
		return
	}
	if calls := nativeToolCalls(res.Events, body.Tools); len(calls) > 0 {
		calls = limitToolCalls(calls, effectiveToolCallLimit(body.ParallelToolCalls, s.settings))
		_ = writeToolResponseWithLimit(r.Context(), w, id, model, false, calls, res, outputLimit)
		return
	}
	// Only gate on tool evidence when tools were actually offered to the
	// model. Plain chat (no tools) legitimately produces "created"/"written"
	// language when writing code or prose — gating those responses was a
	// false positive that replaced real output with an error message.
	if len(body.Tools) > 0 && !completionEvidenceAllows(res.Text, activeLedger) {
		res.Text = "I cannot confirm completion because no matching tool results were returned. No external action has been verified."
	}
	created := time.Now().Unix()
	content := any(res.Text)
	if len(res.Images) > 0 {
		parts := []any{map[string]any{"type": "text", "text": res.Text}}
		for _, u := range res.Images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
		}
		content = parts
	}
	finishReason := "stop"
	if continuationIncomplete || outputTruncated {
		finishReason = "length"
	}
	jsonOut(w, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": finishReason,
		}},
		"m365": compatM365Metadata(res),
	})
}

func backfillTerminalStreamText(text string, delivered *strings.Builder, guarded bool, emit func(string) error) error {
	if delivered == nil || delivered.Len() != 0 || strings.TrimSpace(text) == "" {
		return nil
	}
	if guarded {
		delivered.WriteString(text)
		return nil
	}
	return emit(text)
}

func visibleStreamResumePrompt(partial string) string {
	runes := []rune(partial)
	const maxTailRunes = 16000
	if len(runes) > maxTailRunes {
		runes = runes[len(runes)-maxTailRunes:]
	}
	return "The transport disconnected after the assistant text below was already shown to the user. Continue exactly from the next missing point. Do not repeat, summarize, restart, or mention the disconnection.\n[ALREADY_DELIVERED_ASSISTANT_TAIL]\n" + string(runes)
}

func trimContinuationOverlap(partial, continuation string) string {
	left := []rune(partial)
	right := []rune(continuation)
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	if limit > 2048 {
		limit = 2048
	}
	for overlap := limit; overlap > 0; overlap-- {
		if string(left[len(left)-overlap:]) == string(right[:overlap]) {
			return string(right[overlap:])
		}
	}
	return continuation
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func extractOIDTID(accessToken string) (oid, tid string) {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", ""
	}
	if v, ok := m["oid"].(string); ok {
		oid = v
	}
	if v, ok := m["tid"].(string); ok {
		tid = v
	}
	return oid, tid
}

// --- per-account sequential failover ---

// health returns the per-account health tracker, initializing it once.
func (s *Server) healthPool() *accountHealth {
	s.accountPoolOnce.Do(func() { s.accountPool = newAccountHealth() })
	return s.accountPool
}

// nextHealthyAccount resolves the already-advanced global identity. The failed
// account is advanced by markAccountResult using an active-ID compare-and-swap;
// stale requests therefore cannot choose their own successor or rewind order.
func (s *Server) nextHealthyAccount(avoidID string) (auth.AccountToken, error) {
	return s.resolveActiveAccountWithoutAdvance()
}

// markAccountResult updates per-account health from a chat result/error.
// quotaExhausted reports whether the upstream throttling payload indicates an
// exhausted CostQuota allowance. Copilot attaches a throttling object to every
// response (even successful ones), so the mere presence of the field is not a
// rate-limit signal; only a depleted CostQuota allowance is.
func quotaExhausted(t any) bool {
	m, ok := t.(map[string]any)
	if !ok {
		return false
	}
	if cq, ok := m["CostQuota"].(float64); ok {
		return cq <= 0
	}
	if cq, ok := m["CostQuota"].(map[string]any); ok {
		if ra, ok := cq["remainingAllowance"].(float64); ok {
			return ra <= 0
		}
	}
	metering, ok := m["metering"].(map[string]any)
	if !ok {
		return false
	}
	cq, ok := metering["CostQuota"].(map[string]any)
	if !ok {
		return false
	}
	ra, ok := cq["remainingAllowance"].(float64)
	return ok && ra <= 0
}

// quotaFailureWithoutContent distinguishes a failed, empty quota response from
// a response that successfully consumed the final available allowance. ChatHub
// can report remainingAllowance=0 on that last successful response; treating
// the quota marker alone as failure discards a complete answer and needlessly
// retries it against another account.
func quotaFailureWithoutContent(res chathub.Result, offeredTools ...[]chathub.Tool) bool {
	if isUpstreamThrottlePlaceholderText(firstNonEmpty(res.Text, res.FullText)) {
		return true
	}
	hasReusableContent := strings.TrimSpace(res.Text) != "" || strings.TrimSpace(res.FullText) != "" || len(res.Images) > 0
	if !hasReusableContent && len(offeredTools) > 0 && len(offeredTools[0]) > 0 {
		hasReusableContent = len(nativeToolCalls(res.Events, offeredTools[0])) > 0
	}
	return !hasReusableContent && quotaExhausted(res.Throttling)
}

func strictQuotaExhaustedError() error {
	return &chathub.RateLimitError{Reason: "Microsoft Copilot CostQuota exhausted"}
}

// shouldFailoverAccount is the single policy gate for account rotation. A
// session-derived binding may move, but an explicit accountId is authoritative.
// Liveness frames and SSE headers are not visible model output; text or tool
// deltas are. Each request may rotate at most once.
func shouldFailoverAccount(_ bool, alreadySwitched, visibleOutput bool, err error, res chathub.Result, offeredTools ...[]chathub.Tool) bool {
	if alreadySwitched || visibleOutput {
		return false
	}
	if err != nil {
		return IsUpstreamAccountFailure(err)
	}
	return quotaFailureWithoutContent(res, offeredTools...)
}

func (s *Server) markAccountResult(id string, err error) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	if err == nil {
		s.accountRouteMu.Lock()
		activeID, routeErr := s.ensureActiveAccountLocked(s.tokens.List())
		if routeErr == nil && activeID == id {
			s.healthPool().MarkSuccess(id)
		}
		s.accountRouteMu.Unlock()
		return false
	}
	if IsDisengaged(err) {
		s.accountRouteMu.Lock()
		accounts := s.tokens.List()
		position := accountPositionByID(accounts, id)
		// Rest the identity after an upstream policy refusal. The prompt-scoped
		// refusal cache prevents an automatic retry from replaying the same input
		// on the next account, while a genuinely different request may continue
		// through the deterministic route.
		s.recordAccountFailureHealth(id, err)
		routeErr := s.persistActiveAccountLocked(s.activeAccountID)
		if routeErr != nil {
			log.Printf("[account-router] outcome=persist_error pos=%d class=%s reason=terminal_policy_refusal", position, accountFailureClass(err))
		} else {
			log.Printf("[account-router] outcome=held pos=%d class=%s reason=terminal_policy_refusal", position, accountFailureClass(err))
		}
		s.accountRouteMu.Unlock()
		return false
	}
	if !IsUpstreamAccountFailure(err) {
		return false
	}
	s.accountRouteMu.Lock()
	accounts := s.tokens.List()
	fromPosition := accountPositionByID(accounts, id)
	s.recordAccountFailureHealth(id, err)
	now := time.Now()
	sharedCircuitOpened := s.observeSharedFailureLocked(id, err, now)
	advanced := false
	var routeErr error
	if !sharedCircuitOpened && !s.upstreamCircuitOpenLocked(now) {
		advanced, routeErr = s.advanceActiveAccountLocked(id, accounts)
	}
	if routeErr == nil && !advanced {
		routeErr = s.persistActiveAccountLocked(s.activeAccountID)
	}
	toPosition := accountPositionByID(accounts, s.activeAccountID)
	if routeErr != nil {
		log.Printf("[account-router] outcome=persist_error from_pos=%d class=%s", fromPosition, accountFailureClass(err))
	} else if advanced {
		log.Printf("[account-router] outcome=advanced from_pos=%d to_pos=%d class=%s", fromPosition, toPosition, accountFailureClass(err))
	} else if sharedCircuitOpened || s.upstreamCircuitOpenLocked(time.Now()) {
		log.Printf("[account-router] outcome=held pos=%d class=%s reason=shared_upstream_circuit", fromPosition, accountFailureClass(err))
	} else {
		log.Printf("[account-router] outcome=held pos=%d class=%s reason=no_available_successor_or_stale_result", fromPosition, accountFailureClass(err))
	}
	s.accountRouteMu.Unlock()
	return advanced
}
