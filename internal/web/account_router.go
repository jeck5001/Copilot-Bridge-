package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/vipamess/Copilot-Bridge-/internal/auth"
	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

// accountRouteState is deliberately separate from the token cache. The active
// identity is persisted by account ID (not slice index), so restarts and edits
// to accounts that precede it cannot silently reactivate another identity.
type accountRouteState struct {
	ActiveAccountID string               `json:"activeAccountId"`
	RouteGeneration uint64               `json:"routeGeneration,omitempty"`
	UpdatedAt       time.Time            `json:"updatedAt"`
	Health          accountHealthState   `json:"health,omitempty"`
	PolicyRefusals  map[string]time.Time `json:"policyRefusals,omitempty"`
}

var errInactiveAccount = errors.New("requested account is isolated; only the active account may contact upstream")
var errUpstreamCircuitOpen = errors.New("shared upstream circuit is temporarily open")

const (
	sharedFailureWindow = 12 * time.Second
	upstreamCircuitRest = 20 * time.Second
)

type activeAccountRequestLeaseKey struct{}

// activeAccountRequestLease keeps one logical agent turn on the identity that
// owned it when the turn began. A turn may contain router, repair, answer and
// continuation ChatHub calls; treating those as unrelated leases allowed an
// unrelated concurrent failure to strand the task halfway through. The lease
// is unexported and can only be minted after validating the global active ID.
type activeAccountRequestLease struct {
	accountID  string
	generation uint64
}

func (s *Server) initializeAccountRouter() error {
	var persistedHealth accountHealthState
	var persistedPolicyRefusals map[string]time.Time
	if strings.TrimSpace(s.accountRoutePath) != "" {
		if b, err := os.ReadFile(s.accountRoutePath); err == nil {
			var state accountRouteState
			if err := json.Unmarshal(b, &state); err != nil {
				return fmt.Errorf("decode account route state: %w", err)
			}
			s.activeAccountID = strings.TrimSpace(state.ActiveAccountID)
			s.activeAccountGeneration = state.RouteGeneration
			persistedHealth = state.Health
			persistedPolicyRefusals = state.PolicyRefusals
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read account route state: %w", err)
		}
	}
	// Builds deployed before route generations were persisted may already have
	// generation-tagged sessions. Recover the monotonic floor from that durable
	// data so a restart cannot make a legitimate active-account write look stale.
	if s.sessions != nil {
		if sessionGeneration := s.sessions.maxRouteGeneration(); sessionGeneration > s.activeAccountGeneration {
			s.activeAccountGeneration = sessionGeneration
		}
	}
	s.healthPool().restorePersistedState(persistedHealth)
	s.restorePolicyRefusals(persistedPolicyRefusals)
	s.accountRouteMu.Lock()
	defer s.accountRouteMu.Unlock()
	_, err := s.ensureActiveAccountLocked(s.tokens.List())
	return err
}

func accountIndexByID(list []auth.AccountToken, id string) int {
	for i := range list {
		if list[i].ID == id {
			return i
		}
	}
	return -1
}

func accountPositionByID(list []auth.AccountToken, id string) int {
	index := accountIndexByID(list, id)
	if index < 0 {
		return 0
	}
	return index + 1
}

func accountFailureClass(err error) string {
	switch {
	case IsDisengaged(err):
		return "policy_refusal"
	case IsRateLimited(err):
		return "rate_limit"
	case IsQuotaExhausted(err):
		return "quota_exhausted"
	case IsAuthFailure(err):
		return "auth"
	case IsTransientUpstream(err):
		return "transient_upstream"
	default:
		return "upstream_unknown"
	}
}

func isSharedUpstreamFailure(err error) bool {
	if err == nil || IsDisengaged(err) || IsRateLimited(err) || IsQuotaExhausted(err) || IsAuthFailure(err) || !IsTransientUpstream(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	// An explicitly configured per-account proxy can fail independently; moving
	// to the next fixed identity/route is the correct recovery for that domain.
	for _, marker := range []string{"proxy dialer", "unsupported proxy protocol", "socks5", "http proxy"} {
		if strings.Contains(msg, marker) {
			return false
		}
	}
	return true
}

// observeSharedFailureLocked opens a short process-local breaker only after the
// same infrastructure-shaped failure reaches two distinct identities. This
// prevents a Microsoft/host/network outage from burning through the dormant
// pool in seconds while still allowing a single bad account or proxy to rotate.
func (s *Server) observeSharedFailureLocked(id string, err error, now time.Time) bool {
	if !isSharedUpstreamFailure(err) {
		return false
	}
	if s.sharedFailureAccounts == nil {
		s.sharedFailureAccounts = make(map[string]time.Time)
	}
	for accountID, observedAt := range s.sharedFailureAccounts {
		if now.Sub(observedAt) > sharedFailureWindow {
			delete(s.sharedFailureAccounts, accountID)
		}
	}
	s.sharedFailureAccounts[id] = now
	if len(s.sharedFailureAccounts) < 2 {
		return false
	}
	until := now.Add(upstreamCircuitRest)
	if until.After(s.upstreamCircuitUntil) {
		s.upstreamCircuitUntil = until
	}
	return true
}

func (s *Server) upstreamCircuitOpenLocked(now time.Time) bool {
	return !s.upstreamCircuitUntil.IsZero() && now.Before(s.upstreamCircuitUntil)
}

func (s *Server) persistActiveAccountLocked(id string) error {
	if strings.TrimSpace(s.accountRoutePath) == "" {
		return nil
	}
	b, err := json.MarshalIndent(accountRouteState{
		ActiveAccountID: id,
		RouteGeneration: s.activeAccountGeneration,
		UpdatedAt:       time.Now().UTC(),
		Health:          s.healthPool().persistedState(),
		PolicyRefusals:  s.persistedPolicyRefusals(),
	}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.accountRoutePath, b, 0o600)
}

func (s *Server) ensureActiveAccountLocked(list []auth.AccountToken) (string, error) {
	if len(list) == 0 {
		s.activeAccountID = ""
		return "", nil
	}
	if accountIndexByID(list, s.activeAccountID) >= 0 {
		return s.activeAccountID, nil
	}
	if err := s.activateAccountLocked(list[0].ID); err != nil {
		return "", err
	}
	return s.activeAccountID, nil
}

func (s *Server) cancelActiveLeasesLocked() {
	for leaseID, cancel := range s.activeAccountLeases {
		cancel()
		delete(s.activeAccountLeases, leaseID)
	}
}

func (s *Server) activateAccountLocked(id string) error {
	id = strings.TrimSpace(id)
	if id == s.activeAccountID {
		return nil
	}
	previousID, previousGeneration := s.activeAccountID, s.activeAccountGeneration
	s.activeAccountID = id
	s.activeAccountGeneration++
	if err := s.persistActiveAccountLocked(id); err != nil {
		s.activeAccountID = previousID
		s.activeAccountGeneration = previousGeneration
		return err
	}
	// Existing calls belong to the previous generation and are allowed to drain.
	// Canceling every lease here made an unrelated 429/5xx terminate other
	// already-streaming long tasks. New calls are still isolated because
	// beginActiveAccountCall accepts only the newly active account ID.
	return nil
}

// advanceActiveAccountLocked is a compare-and-swap: only a failure from the
// current identity can advance the router. Late results from older requests can
// update that account's health, but can never move the global pointer backward.
func (s *Server) advanceActiveAccountLocked(failedID string, list []auth.AccountToken) (bool, error) {
	activeID, err := s.ensureActiveAccountLocked(list)
	if err != nil || activeID == "" || activeID != failedID {
		return false, err
	}
	start := accountIndexByID(list, activeID)
	if start < 0 {
		return false, nil
	}
	for offset := 1; offset < len(list); offset++ {
		candidate := list[(start+offset)%len(list)]
		if !s.healthPool().Available(candidate.ID) {
			continue
		}
		if err := s.activateAccountLocked(candidate.ID); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (s *Server) currentActiveAccountID() string {
	s.accountRouteMu.Lock()
	defer s.accountRouteMu.Unlock()
	id, err := s.ensureActiveAccountLocked(s.tokens.List())
	if err != nil {
		return ""
	}
	return id
}

func (s *Server) isActiveAccount(id string) bool {
	return strings.TrimSpace(id) != "" && s.currentActiveAccountID() == strings.TrimSpace(id)
}

func (s *Server) accountRouteVersion(id string) (uint64, bool) {
	s.accountRouteMu.Lock()
	defer s.accountRouteMu.Unlock()
	activeID, err := s.ensureActiveAccountLocked(s.tokens.List())
	if err != nil || strings.TrimSpace(id) == "" || activeID != strings.TrimSpace(id) {
		return s.activeAccountGeneration, false
	}
	return s.activeAccountGeneration, true
}

func (s *Server) refreshActiveAccount(id string) (auth.AccountToken, error) {
	id = strings.TrimSpace(id)
	s.accountRouteMu.Lock()
	list := s.tokens.List()
	activeID, err := s.ensureActiveAccountLocked(list)
	if err != nil {
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, err
	}
	if id == "" || id != activeID {
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, errInactiveAccount
	}
	generation := s.activeAccountGeneration
	s.accountRouteMu.Unlock()

	// Refresh can perform a network request. Never hold the global routing lock
	// while waiting for AAD: unrelated completed streams must still be able to
	// publish health and persistence state.
	tok, err := s.tokens.EnsureValid(activeID)
	s.accountRouteMu.Lock()
	defer s.accountRouteMu.Unlock()
	currentList := s.tokens.List()
	currentID, currentErr := s.ensureActiveAccountLocked(currentList)
	if currentErr != nil {
		return auth.AccountToken{}, currentErr
	}
	if currentID != activeID || s.activeAccountGeneration != generation {
		return auth.AccountToken{}, errInactiveAccount
	}
	if err != nil {
		fromPosition := accountPositionByID(currentList, activeID)
		s.recordAccountFailureHealth(activeID, err)
		advanced, routeErr := s.advanceActiveAccountLocked(activeID, currentList)
		if !advanced {
			_ = s.persistActiveAccountLocked(s.activeAccountID)
		}
		logAccountPreflightRoute("refresh", fromPosition, accountPositionByID(currentList, s.activeAccountID), advanced, routeErr, err)
		return auth.AccountToken{}, err
	}
	// A successful explicit refresh is authoritative evidence that the
	// credential is usable again. Clear only the hard auth pin; retain unrelated
	// quota/policy cooldowns so re-authentication cannot bypass account rest.
	s.healthPool().ClearAuthFailure(activeID)
	if err := s.persistActiveAccountLocked(s.activeAccountID); err != nil {
		return auth.AccountToken{}, err
	}
	return tok, nil
}

func (s *Server) clearAuthFailureAfterCredentialUpdate(id string) error {
	s.accountRouteMu.Lock()
	defer s.accountRouteMu.Unlock()
	s.healthPool().ClearAuthFailure(strings.TrimSpace(id))
	return s.persistActiveAccountLocked(s.activeAccountID)
}

func (s *Server) syncActiveAccount() error {
	s.accountRouteMu.Lock()
	defer s.accountRouteMu.Unlock()
	_, err := s.ensureActiveAccountLocked(s.tokens.List())
	return err
}

// deleteAccountToken keeps account deletion and the active identity update in
// one router transaction. Removing an account before the active one does not
// move the active identity; removing the active one selects its next neighbor
// in the pre-delete order.
func (s *Server) deleteAccountToken(id string) error {
	s.accountRouteMu.Lock()
	defer s.accountRouteMu.Unlock()
	before := s.tokens.List()
	activeID, err := s.ensureActiveAccountLocked(before)
	if err != nil {
		return err
	}
	activeIndex := accountIndexByID(before, activeID)
	if err := s.tokens.Delete(id); err != nil {
		return err
	}
	if id != activeID {
		return nil
	}
	after := s.tokens.List()
	if len(after) == 0 {
		return s.activateAccountLocked("")
	}
	if activeIndex < 0 {
		activeIndex = 0
	}
	return s.activateAccountLocked(after[activeIndex%len(after)].ID)
}

// resolveActiveAccount validates exactly one identity. It never scans and
// refreshes several dormant accounts in one request. A failed active token is
// retired in sequence; the caller may make one explicit attempt on the new
// active identity.
func (s *Server) resolveActiveAccount() (auth.AccountToken, error) {
	s.accountRouteMu.Lock()
	list := s.tokens.List()
	activeID, err := s.ensureActiveAccountLocked(list)
	if err != nil {
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, err
	}
	if activeID == "" {
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, fmt.Errorf("no accounts; login first")
	}
	if s.upstreamCircuitOpenLocked(time.Now()) {
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, errUpstreamCircuitOpen
	}
	if !s.healthPool().Available(activeID) {
		fromPosition := accountPositionByID(list, activeID)
		advanced, advanceErr := s.advanceActiveAccountLocked(activeID, list)
		logAccountPreflightRoute("health_cooldown", fromPosition, accountPositionByID(list, s.activeAccountID), advanced, advanceErr, nil)
		s.accountRouteMu.Unlock()
		if advanceErr != nil {
			return auth.AccountToken{}, advanceErr
		}
		return auth.AccountToken{}, errPinnedAccountUnavailable
	}
	generation := s.activeAccountGeneration
	s.accountRouteMu.Unlock()

	tok, err := s.tokens.EnsureValid(activeID)
	s.accountRouteMu.Lock()
	currentList := s.tokens.List()
	currentID, currentErr := s.ensureActiveAccountLocked(currentList)
	if currentErr != nil {
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, currentErr
	}
	if currentID != activeID || s.activeAccountGeneration != generation {
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, errInactiveAccount
	}
	if err != nil {
		fromPosition := accountPositionByID(currentList, activeID)
		s.recordAccountFailureHealth(activeID, err)
		advanced, advanceErr := s.advanceActiveAccountLocked(activeID, currentList)
		if advanceErr == nil && !advanced {
			advanceErr = s.persistActiveAccountLocked(s.activeAccountID)
		}
		logAccountPreflightRoute("token_validation", fromPosition, accountPositionByID(currentList, s.activeAccountID), advanced, advanceErr, err)
		s.accountRouteMu.Unlock()
		if advanceErr != nil {
			return auth.AccountToken{}, advanceErr
		}
		return auth.AccountToken{}, err
	}
	s.accountRouteMu.Unlock()

	s.mu.Lock()
	s.accountStats[activeID]++
	s.statsDirty = true
	s.mu.Unlock()
	return tok, nil
}

// resolveActiveAccountWithoutAdvance validates the active identity selected by
// a previous preflight failure without consuming another routing slot. A
// single client request is allowed to move the global account pointer only
// once; if this replacement is also unhealthy it is quarantined and the next
// request will advance from it in sequence.
func (s *Server) resolveActiveAccountWithoutAdvance() (auth.AccountToken, error) {
	s.accountRouteMu.Lock()
	list := s.tokens.List()
	activeID, err := s.ensureActiveAccountLocked(list)
	if err != nil {
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, err
	}
	if activeID == "" {
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, fmt.Errorf("no accounts; login first")
	}
	if s.upstreamCircuitOpenLocked(time.Now()) {
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, errUpstreamCircuitOpen
	}
	if !s.healthPool().Available(activeID) {
		_ = s.persistActiveAccountLocked(activeID)
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, errPinnedAccountUnavailable
	}
	generation := s.activeAccountGeneration
	s.accountRouteMu.Unlock()

	tok, err := s.tokens.EnsureValid(activeID)
	s.accountRouteMu.Lock()
	currentList := s.tokens.List()
	currentID, currentErr := s.ensureActiveAccountLocked(currentList)
	if currentErr != nil {
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, currentErr
	}
	if currentID != activeID || s.activeAccountGeneration != generation {
		s.accountRouteMu.Unlock()
		return auth.AccountToken{}, errInactiveAccount
	}
	if err != nil {
		s.recordAccountFailureHealth(activeID, err)
		persistErr := s.persistActiveAccountLocked(activeID)
		logAccountPreflightRoute("token_validation_retry", accountPositionByID(currentList, activeID), accountPositionByID(currentList, activeID), false, persistErr, err)
		s.accountRouteMu.Unlock()
		if persistErr != nil {
			return auth.AccountToken{}, persistErr
		}
		return auth.AccountToken{}, err
	}
	s.accountRouteMu.Unlock()

	s.mu.Lock()
	s.accountStats[activeID]++
	s.statsDirty = true
	s.mu.Unlock()
	return tok, nil
}

func (s *Server) recordAccountFailureHealth(id string, err error) {
	if IsDisengaged(err) {
		s.healthPool().MarkDisengaged(id)
	} else if IsQuotaExhausted(err) {
		s.healthPool().MarkQuotaExhausted(id, rateLimitRetryUntil(err))
	} else if IsRateLimited(err) {
		s.healthPool().MarkRateLimited(id, rateLimitRetryUntil(err))
	} else if IsAuthFailure(err) {
		s.healthPool().MarkAuthFail(id)
	} else {
		s.healthPool().MarkTransient(id)
	}
}

// recordAccountFailureWithoutAdvance quarantines a failed retry identity while
// preserving the current route. This is the terminal-failure half of the
// per-request switch budget: the next independent request may advance one
// position, but the current request cannot burn through the dormant pool.
func (s *Server) recordAccountFailureWithoutAdvance(id string, err error) {
	id = strings.TrimSpace(id)
	if id == "" || (!IsUpstreamAccountFailure(err) && !IsDisengaged(err)) {
		return
	}
	s.accountRouteMu.Lock()
	accounts := s.tokens.List()
	position := accountPositionByID(accounts, id)
	s.recordAccountFailureHealth(id, err)
	sharedCircuitOpened := s.observeSharedFailureLocked(id, err, time.Now())
	routeErr := s.persistActiveAccountLocked(s.activeAccountID)
	if routeErr != nil {
		log.Printf("[account-router] outcome=persist_error pos=%d class=%s reason=request_switch_budget_exhausted", position, accountFailureClass(err))
	} else if sharedCircuitOpened {
		log.Printf("[account-router] outcome=held pos=%d class=%s reason=shared_upstream_circuit", position, accountFailureClass(err))
	} else {
		log.Printf("[account-router] outcome=held pos=%d class=%s reason=request_switch_budget_exhausted", position, accountFailureClass(err))
	}
	s.accountRouteMu.Unlock()
}

func logAccountPreflightRoute(reason string, fromPosition, toPosition int, advanced bool, routeErr, failure error) {
	class := "cooldown"
	if failure != nil {
		class = accountFailureClass(failure)
	}
	if routeErr != nil {
		log.Printf("[account-router] outcome=persist_error from_pos=%d class=%s reason=%s", fromPosition, class, reason)
	} else if advanced {
		log.Printf("[account-router] outcome=advanced from_pos=%d to_pos=%d class=%s reason=%s", fromPosition, toPosition, class, reason)
	} else {
		log.Printf("[account-router] outcome=held pos=%d class=%s reason=%s_no_available_successor", fromPosition, class, reason)
	}
}

func (s *Server) markAccountSuccess(id string, _ any) {
	s.markAccountResult(id, nil)
}

func (s *Server) beginActiveAccountCall(parent context.Context, accountID string) (context.Context, func(), error) {
	if lease, ok := parent.Value(activeAccountRequestLeaseKey{}).(activeAccountRequestLease); ok {
		if strings.TrimSpace(accountID) != "" && lease.accountID == strings.TrimSpace(accountID) {
			return parent, func() {}, nil
		}
	}
	s.accountRouteMu.Lock()
	activeID, err := s.ensureActiveAccountLocked(s.tokens.List())
	if err != nil {
		s.accountRouteMu.Unlock()
		return nil, nil, err
	}
	if accountID == "" || accountID != activeID {
		s.accountRouteMu.Unlock()
		return nil, nil, errInactiveAccount
	}
	ctx, cancel := context.WithCancel(parent)
	s.nextAccountLeaseID++
	leaseID := s.nextAccountLeaseID
	if s.activeAccountLeases == nil {
		s.activeAccountLeases = make(map[uint64]context.CancelFunc)
	}
	s.activeAccountLeases[leaseID] = cancel
	s.accountRouteMu.Unlock()

	release := func() {
		s.accountRouteMu.Lock()
		if registered, ok := s.activeAccountLeases[leaseID]; ok {
			delete(s.activeAccountLeases, leaseID)
			registered()
		}
		s.accountRouteMu.Unlock()
	}
	return ctx, release, nil
}

func (s *Server) beginActiveAccountRequest(parent context.Context, accountID string) (context.Context, func(), error) {
	accountID = strings.TrimSpace(accountID)
	s.accountRouteMu.Lock()
	activeID, err := s.ensureActiveAccountLocked(s.tokens.List())
	if err != nil {
		s.accountRouteMu.Unlock()
		return nil, nil, err
	}
	if accountID == "" || accountID != activeID {
		s.accountRouteMu.Unlock()
		return nil, nil, errInactiveAccount
	}
	ctx, cancel := context.WithCancel(parent)
	s.nextAccountLeaseID++
	leaseID := s.nextAccountLeaseID
	if s.activeAccountLeases == nil {
		s.activeAccountLeases = make(map[uint64]context.CancelFunc)
	}
	s.activeAccountLeases[leaseID] = cancel
	lease := activeAccountRequestLease{accountID: accountID, generation: s.activeAccountGeneration}
	s.accountRouteMu.Unlock()
	ctx = context.WithValue(ctx, activeAccountRequestLeaseKey{}, lease)

	release := func() {
		s.accountRouteMu.Lock()
		if registered, ok := s.activeAccountLeases[leaseID]; ok {
			delete(s.activeAccountLeases, leaseID)
			registered()
		}
		s.accountRouteMu.Unlock()
	}
	return ctx, release, nil
}

func (s *Server) chatActive(ctx context.Context, accountID string, account chathub.Account, req chathub.Request) (chathub.Result, error) {
	callCtx, release, err := s.beginActiveAccountCall(ctx, accountID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	return s.chat.Chat(callCtx, account, req)
}

func (s *Server) chatActiveWithDelta(ctx context.Context, accountID string, account chathub.Account, req chathub.Request, onDelta func(string) error) (chathub.Result, error) {
	callCtx, release, err := s.beginActiveAccountCall(ctx, accountID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	return s.chat.ChatWithDelta(callCtx, account, req, onDelta)
}

func (s *Server) chatActiveWithEvents(ctx context.Context, accountID string, account chathub.Account, req chathub.Request, handler chathub.StreamHandler) (chathub.Result, error) {
	callCtx, release, err := s.beginActiveAccountCall(ctx, accountID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	return s.chat.ChatWithEvents(callCtx, account, req, handler)
}
