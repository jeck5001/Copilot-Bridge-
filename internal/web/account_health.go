package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

// accountHealth tracks per-account upstream failures so the gateway can skip
// accounts that are currently rate-limited or auth-broken instead of hammering
// them. It backs the per-account 429 failover (D 项).
type accountHealth struct {
	mu        sync.Mutex
	cooldown  map[string]time.Time // rate-limit cooldown deadline per account
	transient map[string]time.Time // short cooldown for network/upstream instability
	authFail  map[string]bool      // hard pin: account auth broken, skip until re-login
}

type accountHealthState struct {
	Cooldown  map[string]time.Time `json:"cooldown,omitempty"`
	Transient map[string]time.Time `json:"transient,omitempty"`
	AuthFail  map[string]bool      `json:"authFail,omitempty"`
}

func newAccountHealth() *accountHealth {
	return &accountHealth{
		cooldown:  make(map[string]time.Time),
		transient: make(map[string]time.Time),
		authFail:  make(map[string]bool),
	}
}

func (h *accountHealth) persistedState() accountHealthState {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	state := accountHealthState{
		Cooldown:  make(map[string]time.Time),
		Transient: make(map[string]time.Time),
		AuthFail:  make(map[string]bool),
	}
	for id, until := range h.cooldown {
		if now.Before(until) {
			state.Cooldown[id] = until
		}
	}
	for id, until := range h.transient {
		if now.Before(until) {
			state.Transient[id] = until
		}
	}
	for id, failed := range h.authFail {
		if failed {
			state.AuthFail[id] = true
		}
	}
	return state
}

func (h *accountHealth) restorePersistedState(state accountHealthState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	h.cooldown = make(map[string]time.Time)
	h.transient = make(map[string]time.Time)
	h.authFail = make(map[string]bool)
	for id, until := range state.Cooldown {
		if strings.TrimSpace(id) != "" && now.Before(until) {
			h.cooldown[id] = until
		}
	}
	for id, until := range state.Transient {
		if strings.TrimSpace(id) != "" && now.Before(until) {
			h.transient[id] = until
		}
	}
	for id, failed := range state.AuthFail {
		if strings.TrimSpace(id) != "" && failed {
			h.authFail[id] = true
		}
	}
}

// rateLimitCooldown is how long a rate-limited account is skipped before it may
// be selected again.
const rateLimitCooldown = 2 * time.Minute

// CostQuota exhaustion is not a short transport throttle. Microsoft often
// supplies no reset timestamp for it, so retrying the account again after two
// minutes repeatedly burns requests and increases account risk. Keep it rested
// for a conservative window unless the upstream supplies an explicit reset.
const quotaExhaustedCooldown = 6 * time.Hour

// transientFailureCooldown is deliberately short: a broken proxy or dropped
// WebSocket should fail over immediately, but the account may be healthy again
// on a later request.
const transientFailureCooldown = 30 * time.Second

func extendDeadline(deadlines map[string]time.Time, id string, until time.Time) {
	if current, ok := deadlines[id]; !ok || until.After(current) {
		deadlines[id] = until
	}
}

// MarkRateLimited puts the account on cooldown until the given time. A zero
// time defaults to now+rateLimitCooldown.
func (h *accountHealth) MarkRateLimited(id string, until time.Time) {
	if until.IsZero() {
		until = time.Now().Add(rateLimitCooldown)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	extendDeadline(h.cooldown, id, until)
	// Authentication failure is a stronger state. A late 429 from another
	// in-flight request must not downgrade a credential that requires re-login.
}

func (h *accountHealth) MarkQuotaExhausted(id string, until time.Time) {
	if until.IsZero() {
		until = time.Now().Add(quotaExhaustedCooldown)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	extendDeadline(h.cooldown, id, until)
}

func (h *accountHealth) MarkTransient(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	extendDeadline(h.transient, id, time.Now().Add(transientFailureCooldown))
}

// MarkAuthFail pins the account as auth-broken (e.g. 401/403). It is skipped
// until an explicit credential refresh/replacement calls ClearAuthFailure.
func (h *accountHealth) MarkAuthFail(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authFail[id] = true
}

// MarkSuccess cleans only expired timed state. It deliberately cannot erase a
// concurrent/newer failure or the hard authentication pin.
func (h *accountHealth) MarkSuccess(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Do not let an older in-flight success clear any newer health signal. Only
	// expired timed failures may be cleaned here; an auth failure requires an
	// explicit credential refresh/replacement via ClearAuthFailure.
	if until, ok := h.cooldown[id]; !ok || !time.Now().Before(until) {
		delete(h.cooldown, id)
	}
	if until, ok := h.transient[id]; !ok || !time.Now().Before(until) {
		delete(h.transient, id)
	}
}

// ClearAuthFailure is called only after a successful credential replacement or
// refresh. Ordinary late successes and weaker health signals cannot clear an
// authentication failure.
func (h *accountHealth) ClearAuthFailure(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.authFail, id)
}

// Available reports whether the account can be selected for a new request.
func (h *accountHealth) Available(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.authFail[id] {
		return false
	}
	if t, ok := h.cooldown[id]; ok && time.Now().Before(t) {
		return false
	}
	if t, ok := h.transient[id]; ok && time.Now().Before(t) {
		return false
	}
	return true
}

// accountHealthView is the per-account health snapshot for the admin UI.
type accountHealthView struct {
	AccountID     string `json:"accountId"`
	RateLimited   bool   `json:"rateLimited"`
	AuthFail      bool   `json:"authFail"`
	TransientFail bool   `json:"transientFail"`
	CooldownUntil string `json:"cooldownUntil,omitempty"`
}

// Snapshot returns a copy of the current health state.
func (h *accountHealth) Snapshot() []accountHealthView {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]accountHealthView, 0, len(h.cooldown)+len(h.transient)+len(h.authFail))
	now := time.Now()
	for id, t := range h.cooldown {
		out = append(out, accountHealthView{
			AccountID:     id,
			RateLimited:   now.Before(t),
			CooldownUntil: t.Format(time.RFC3339),
		})
	}
	for id, t := range h.transient {
		out = append(out, accountHealthView{
			AccountID:     id,
			TransientFail: now.Before(t),
			CooldownUntil: t.Format(time.RFC3339),
		})
	}
	for id := range h.authFail {
		out = append(out, accountHealthView{AccountID: id, AuthFail: true})
	}
	return out
}

var (
	legacyHTTP429  = regexp.MustCompile(`(?i)(^|[^0-9])429([^0-9]|$)`)
	legacyHTTPAuth = regexp.MustCompile(`(?i)(^|[^0-9])(?:401|403)([^0-9]|$)`)
)

// IsRateLimited reports only a confirmed HTTP 429. Ordinary 5xx, proxy,
// WebSocket, auth, disengaged and fuzzy "throttled" text must never move a
// conversation to another account. The boundary-matched text check is kept
// solely for older chathub errors that predate RateLimitError.
func IsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var rateLimit *chathub.RateLimitError
	if errors.As(err, &rateLimit) {
		return rateLimit.StatusCode == http.StatusTooManyRequests
	}
	return legacyHTTP429.MatchString(err.Error())
}

// IsQuotaExhausted is deliberately narrower than IsRateLimited: it accepts
// only the typed empty-content CostQuota signal produced by chathub. Generic
// words such as "quota" or "throttled" are not sufficient to rotate accounts.
func IsQuotaExhausted(err error) bool {
	if err == nil {
		return false
	}
	var rateLimit *chathub.RateLimitError
	if !errors.As(err, &rateLimit) || rateLimit.StatusCode != 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(rateLimit.Reason), "Microsoft Copilot CostQuota exhausted")
}

func rateLimitRetryUntil(err error) time.Time {
	var rateLimit *chathub.RateLimitError
	if !errors.As(err, &rateLimit) {
		return time.Time{}
	}
	if !rateLimit.RetryAt.IsZero() {
		return rateLimit.RetryAt
	}
	if rateLimit.RetryAfter > 0 {
		return time.Now().Add(rateLimit.RetryAfter)
	}
	return time.Time{}
}

// IsAuthFailure reports whether err represents an auth failure (401/403).
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	var statusError *chathub.HTTPStatusError
	if errors.As(err, &statusError) {
		return statusError.StatusCode == http.StatusUnauthorized || statusError.StatusCode == http.StatusForbidden
	}
	msg := strings.ToLower(err.Error())
	if legacyHTTPAuth.MatchString(msg) {
		return true
	}
	for _, kw := range []string{"unauthorized", "forbidden", "invalid_grant", "token expired", "authentication failed"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// disengagedCooldown reflects the observed account-degradation recovery time:
// sustained heavy use makes the upstream safety filter fire far more readily
// and the account self-heals only after roughly a 15-minute lull. Retrying
// sooner re-disengages and burns conversation quota.
const disengagedCooldown = 12 * time.Minute

// MarkDisengaged puts the account into a long cooldown after an upstream
// safety-filter refusal. Distinct from rate limiting: re-auth does not clear
// it, only resting does.
func (h *accountHealth) MarkDisengaged(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	extendDeadline(h.transient, id, time.Now().Add(disengagedCooldown))
	// Preserve a stronger auth failure recorded by another in-flight request.
}

// IsTransientUpstream reports failures that are commonly tied to one proxy,
// route, or short-lived upstream connection. They are safe to retry on a
// different account only before any response bytes have reached the caller.
func IsTransientUpstream(err error) bool {
	if err == nil {
		return false
	}
	// A request-owned deadline is a local cancellation boundary, not evidence
	// that the selected Microsoft identity or route is unhealthy. ChatHub read
	// deadlines are returned with their own "ws read"/"response deadline"
	// markers and remain transient upstream failures below.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{
		"unexpected eof", "eof", "close 1006", "abnormal closure",
		"broken pipe", "connection reset", "connection refused",
		"i/o timeout", "tls handshake timeout", "ws dial",
		"ws read before completion", "returned no content", "chathub completion error",
		"chathub upstream error", "chathub closed before completion", "chathub response deadline",
		"upstream error", "bad gateway", "service unavailable", "gateway timeout",
		"internal server error", "http 500", "http 502", "http 503", "http 504",
		"proxy dialer", "unsupported proxy protocol", "network is unreachable", "no route to host",
		"no such host", "server misbehaving", "x509:", "certificate", "tls:",
	} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// IsUpstreamAccountFailure is the only class allowed to advance the active
// account. Request validation, client cancellation, downstream write errors
// and persistence failures are intentionally absent and must remain local.
func IsUpstreamAccountFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrClosedPipe) {
		return false
	}
	// Disengaged is a terminal policy decision for the current prompt. Retrying
	// the same prompt under another identity is both semantically wrong and a
	// needless account-risk amplifier. Routing rests the selected identity, and
	// the prompt-scoped refusal cache blocks identical retries before routing.
	if IsDisengaged(err) {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if (strings.Contains(msg, "context deadline exceeded") || strings.TrimSpace(msg) == "deadline exceeded") &&
		!strings.Contains(msg, "ws read") && !strings.Contains(msg, "chathub response deadline") && !strings.Contains(msg, "i/o timeout") {
		return false
	}
	for _, local := range []string{"invalid request", "persist session", "session persistence"} {
		if strings.Contains(msg, local) {
			return false
		}
	}
	// Call sites pass errors returned by ChatHub/account operations. Unknown
	// upstream failures therefore fail closed into rotation; maintaining an
	// exhaustive message list previously pinned accounts on new backend errors.
	return true
}

// IsDisengaged reports the upstream safety-filter refusal. The gateway must
// return it as terminal and must not replay the prompt under another identity.
func IsDisengaged(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, chathub.ErrDisengaged) || strings.Contains(strings.ToLower(err.Error()), "chathub disengaged")
}
