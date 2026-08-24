package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vipamess/Copilot-Bridge-/internal/auth"
	"github.com/vipamess/Copilot-Bridge-/internal/chathub"
)

func TestStickyAccountModeDefaultsOn(t *testing.T) {
	for _, value := range []string{"", "false", "0", "no", "off", "true", "1", "yes", "on"} {
		t.Run("single active is immutable "+value, func(t *testing.T) {
			t.Setenv("M365_STICKY_ACCOUNT", value)
			if !stickyAccountsEnabled() {
				t.Fatalf("M365_STICKY_ACCOUNT=%q re-enabled per-request account rotation", value)
			}
		})
	}
}

func newStickyAccountTestServer(t *testing.T, ids ...string) *Server {
	t.Helper()
	t.Setenv("M365_STICKY_ACCOUNT", "true")
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := store.Upsert(auth.TokenSet{
			AccessToken: "token-" + id,
			HomeOID:     id,
			TenantID:    "tenant-" + id,
			Email:       id + "@example.test",
			ExpiresAt:   time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("upsert account %s: %v", id, err)
		}
	}
	return &Server{
		tokens:          store,
		accountStats:    make(map[string]int64),
		accountTokenIn:  make(map[string]int64),
		accountTokenOut: make(map[string]int64),
	}
}

func TestStrictRateLimitClassification(t *testing.T) {
	typed429 := &chathub.RateLimitError{StatusCode: http.StatusTooManyRequests, Reason: "capacity reached"}
	for name, err := range map[string]error{
		"typed HTTP 429":           typed429,
		"wrapped typed HTTP 429":   fmt.Errorf("request failed: %w", typed429),
		"legacy boundary HTTP 429": errors.New("upstream returned HTTP 429"),
	} {
		t.Run(name, func(t *testing.T) {
			if !IsRateLimited(err) {
				t.Fatalf("confirmed 429 was not recognized: %T %v", err, err)
			}
		})
	}

	for name, err := range map[string]error{
		"quota text":                 errors.New("quota exhausted"),
		"throttled text":             errors.New("request was throttled"),
		"rate limit text":            errors.New("rate limit exceeded"),
		"too many requests text":     errors.New("too many requests"),
		"HTTP 503":                   errors.New("HTTP 503 service unavailable"),
		"network EOF":                errors.New("ws read before completion: unexpected EOF"),
		"connection reset":           errors.New("read: connection reset by peer"),
		"typed HTTP 503":             &chathub.RateLimitError{StatusCode: http.StatusServiceUnavailable, Reason: "body mentions 429"},
		"429 inside a larger number": errors.New("diagnostic code 14299"),
	} {
		t.Run(name, func(t *testing.T) {
			if IsRateLimited(err) {
				t.Fatalf("non-429 error was classified as 429: %T %v", err, err)
			}
		})
	}
}

func TestReplacementTokenFailureDoesNotConsumeSecondRoutingSlot(t *testing.T) {
	server := newStickyAccountTestServer(t, "account-1", "account-2", "account-3")
	if _, err := server.tokens.Upsert(auth.TokenSet{
		AccessToken: "token-account-2",
		HomeOID:     "account-2",
		TenantID:    "tenant-account-2",
		Email:       "account-2@example.test",
		ExpiresAt:   time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	server.markAccountResult("account-1", &chathub.RateLimitError{StatusCode: http.StatusTooManyRequests, Reason: "rate limited"})
	if active := server.activeAccountID; active != "account-2" {
		t.Fatalf("first failure did not advance to account-2: %s", active)
	}
	if _, err := server.nextHealthyAccount("account-1"); err == nil {
		t.Fatal("expected replacement token validation failure")
	}
	if active := server.activeAccountID; active != "account-2" {
		t.Fatalf("replacement validation consumed a second routing slot: %s", active)
	}
	if calls := server.accountStats["account-3"]; calls != 0 {
		t.Fatalf("account-3 was touched by the same request: %d", calls)
	}
}

func TestQuotaExhaustionClassificationIsTypedOnly(t *testing.T) {
	typed := &chathub.RateLimitError{Reason: "Microsoft Copilot CostQuota exhausted"}
	if !IsQuotaExhausted(typed) {
		t.Fatal("typed empty CostQuota exhaustion was not recognized")
	}
	if !IsQuotaExhausted(fmt.Errorf("wrapped: %w", typed)) {
		t.Fatal("wrapped typed CostQuota exhaustion was not recognized")
	}
	for name, err := range map[string]error{
		"ordinary quota text":              errors.New("Microsoft Copilot CostQuota exhausted"),
		"ordinary throttled text":          errors.New("quota throttled"),
		"typed generic quota":              &chathub.RateLimitError{Reason: "quota exhausted"},
		"typed HTTP 503 with exact reason": &chathub.RateLimitError{StatusCode: http.StatusServiceUnavailable, Reason: "Microsoft Copilot CostQuota exhausted"},
	} {
		t.Run(name, func(t *testing.T) {
			if IsQuotaExhausted(err) {
				t.Fatalf("non-structured quota signal was accepted: %T %v", err, err)
			}
		})
	}
}

func TestShouldFailoverAccountPolicy(t *testing.T) {
	typed429 := &chathub.RateLimitError{StatusCode: http.StatusTooManyRequests, Reason: "rate limited"}
	typedQuota := &chathub.RateLimitError{Reason: "Microsoft Copilot CostQuota exhausted"}
	emptyQuota := chathub.Result{Throttling: map[string]any{
		"metering": map[string]any{
			"CostQuota": map[string]any{"remainingAllowance": float64(0)},
		},
	}}
	completeAtBoundary := emptyQuota
	completeAtBoundary.Text = "last complete answer"

	tests := []struct {
		name            string
		explicitAccount bool
		alreadySwitched bool
		visibleOutput   bool
		err             error
		result          chathub.Result
		want            bool
	}{
		{name: "typed 429 before output", err: typed429, want: true},
		{name: "wrapped typed 429 before output", err: fmt.Errorf("wrapped: %w", typed429), want: true},
		{name: "typed CostQuota before output", err: typedQuota, want: true},
		{name: "empty structured CostQuota result", result: emptyQuota, want: true},
		{name: "completed answer at quota boundary", result: completeAtBoundary, want: false},
		{name: "plain quota upstream error", err: errors.New("quota exhausted"), want: true},
		{name: "plain throttled upstream error", err: errors.New("throttled"), want: true},
		{name: "unknown upstream error", err: errors.New("new backend failure code"), want: true},
		{name: "HTTP 401", err: errors.New("HTTP 401 unauthorized"), want: true},
		{name: "HTTP 403", err: errors.New("HTTP 403 forbidden"), want: true},
		{name: "HTTP 500", err: errors.New("HTTP 500 internal server error"), want: true},
		{name: "HTTP 503", err: errors.New("HTTP 503 service unavailable"), want: true},
		{name: "request deadline exceeded", err: context.DeadlineExceeded, want: false},
		{name: "stringified request deadline", err: errors.New("operation failed: context deadline exceeded"), want: false},
		{name: "upstream websocket read deadline", err: errors.New("ws read before completion: read tcp: i/o timeout"), want: true},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF, want: true},
		{name: "websocket 1006", err: errors.New("websocket: close 1006 abnormal closure"), want: true},
		{name: "proxy failure", err: errors.New("proxy dialer: connection refused"), want: true},
		{name: "proxy configuration failure", err: errors.New("proxy dialer: unsupported proxy protocol"), want: true},
		{name: "TLS verification failure", err: errors.New("tls: failed to verify certificate"), want: true},
		{name: "DNS failure", err: errors.New("dial tcp: lookup upstream: no such host"), want: true},
		{name: "generic chathub failure", err: errors.New("chathub completion error: backend failed"), want: true},
		{name: "terminal policy refusal", err: chathub.ErrDisengaged, want: false},
		{name: "client cancellation", err: context.Canceled, want: false},
		{name: "downstream write failure", err: io.ErrClosedPipe, want: false},
		{name: "input validation", err: errors.New("invalid request payload"), want: false},
		{name: "session persistence", err: errors.New("persist session: access denied"), want: false},
		{name: "explicit active account cannot pin failed identity", explicitAccount: true, err: typed429, want: true},
		{name: "single request cannot switch twice", alreadySwitched: true, err: typed429, want: false},
		{name: "visible output forbids replay", visibleOutput: true, err: typed429, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldFailoverAccount(tc.explicitAccount, tc.alreadySwitched, tc.visibleOutput, tc.err, tc.result)
			if got != tc.want {
				t.Fatalf("shouldFailoverAccount=%t want=%t", got, tc.want)
			}
		})
	}
}

func TestVisibleUpstreamFailureAdvancesNextRequestWithoutReplay(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b", "account-c")
	first, err := s.resolveAccount("")
	if err != nil || first.ID != "account-a" {
		t.Fatalf("initial account=%q err=%v", first.ID, err)
	}
	failure := errors.New("ws read before completion: websocket close 1006 abnormal closure")
	if shouldFailoverAccount(false, false, true, failure, chathub.Result{}) {
		t.Fatal("visible output must not be replayed on another account")
	}

	// The current response is not replayed, but the failed account is retired
	// for the following request.
	s.markAccountResult(first.ID, failure)
	next, err := s.resolveAccount("")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "account-b" {
		t.Fatalf("next request used %s; want account-b", next.ID)
	}
}

func TestStickyAccountDoesNotRoundRobin(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b", "account-c")
	for i := 0; i < 20; i++ {
		account, err := s.resolveAccount("")
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if account.ID != "account-a" {
			t.Fatalf("request %d used %s; sticky account-a must not round-robin", i, account.ID)
		}
	}
}

func TestSticky429PinsNextAccount(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b", "account-c")
	first, err := s.resolveAccount("")
	if err != nil || first.ID != "account-a" {
		t.Fatalf("initial account=%q err=%v", first.ID, err)
	}
	s.markAccountResult(first.ID, &chathub.RateLimitError{StatusCode: http.StatusTooManyRequests, Reason: "rate limited"})

	for i := 0; i < 20; i++ {
		account, err := s.resolveAccount("")
		if err != nil {
			t.Fatalf("resolve after 429 %d: %v", i, err)
		}
		if account.ID != "account-b" {
			t.Fatalf("request %d after 429 used %s; account-b must remain active", i, account.ID)
		}
	}
}

func TestStickyUpstreamFailuresAdvanceAndRemainPinned(t *testing.T) {
	failures := map[string]error{
		"401":                 errors.New("HTTP 401 unauthorized"),
		"403":                 errors.New("HTTP 403 forbidden"),
		"500":                 errors.New("HTTP 500 internal server error"),
		"503":                 errors.New("HTTP 503 service unavailable"),
		"EOF":                 io.ErrUnexpectedEOF,
		"websocket 1006":      errors.New("websocket: close 1006 abnormal closure"),
		"proxy failure":       errors.New("proxy dialer: connection refused"),
		"proxy configuration": errors.New("proxy dialer: unsupported proxy protocol"),
		"TLS verification":    errors.New("tls: failed to verify certificate"),
		"DNS":                 errors.New("dial tcp: lookup upstream: no such host"),
		"generic chathub":     errors.New("chathub completion error: backend failed"),
		"typed 429":           &chathub.RateLimitError{StatusCode: http.StatusTooManyRequests, Reason: "rate limited"},
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			s := newStickyAccountTestServer(t, "account-a", "account-b", "account-c")
			first, err := s.resolveAccount("")
			if err != nil || first.ID != "account-a" {
				t.Fatalf("initial account=%q err=%v", first.ID, err)
			}
			s.markAccountResult(first.ID, failure)
			for i := 0; i < 10; i++ {
				next, err := s.resolveAccount("")
				if err != nil {
					t.Fatalf("resolve %d after failure: %v", i, err)
				}
				if next.ID != "account-b" {
					t.Fatalf("request %d after %s used %s; want account-b", i, name, next.ID)
				}
			}
		})
	}
}

func TestLocalFailuresDoNotAdvanceStickyAccount(t *testing.T) {
	failures := map[string]error{
		"client cancellation": context.Canceled,
		"request deadline":    context.DeadlineExceeded,
		"downstream write":    io.ErrClosedPipe,
		"input validation":    errors.New("invalid request payload"),
		"session persistence": errors.New("persist session: access denied"),
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			s := newStickyAccountTestServer(t, "account-a", "account-b")
			first, err := s.resolveAccount("")
			if err != nil || first.ID != "account-a" {
				t.Fatalf("initial account=%q err=%v", first.ID, err)
			}
			s.markAccountResult(first.ID, failure)
			next, err := s.resolveAccount("")
			if err != nil {
				t.Fatal(err)
			}
			if next.ID != "account-a" {
				t.Fatalf("local %s advanced to %s", name, next.ID)
			}
		})
	}
}

func TestDisengagedDoesNotRotateInsideRequest(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b")
	if s.markAccountResult("account-a", chathub.ErrDisengaged) {
		t.Fatal("terminal policy refusal must not be replayed under another identity")
	}
	if active := s.currentActiveAccountID(); active != "account-a" {
		t.Fatalf("policy refusal changed active identity to %s", active)
	}
	if s.healthPool().Available("account-a") {
		t.Fatalf("policy-refusing identity was not rested: %+v", s.healthPool().Snapshot())
	}
}

func TestDisengagedRestsIdentityAndIdenticalPromptIsBlocked(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b")
	const refused = "developer contract\nuser task"
	s.rememberPolicyRefusal(refused)
	s.recordAccountFailureWithoutAdvance("account-a", chathub.ErrDisengaged)
	if !s.recentPolicyRefusal(refused) {
		t.Fatal("identical refused prompt was not blocked")
	}
	if s.recentPolicyRefusal("a genuinely different task") {
		t.Fatal("different prompt was incorrectly blocked")
	}
	next, switched, err := s.resolveRequestAccount("", false)
	if err != nil {
		t.Fatal(err)
	}
	if !switched {
		t.Fatal("rested identity did not produce a preflight sequential switch")
	}
	if next.ID != "account-b" {
		t.Fatalf("different request did not advance sequentially to account-b: %s", next.ID)
	}
	if s.healthPool().Available("account-a") {
		t.Fatalf("policy-refusing identity was not isolated: %+v", s.healthPool().Snapshot())
	}
}

func TestConcurrent429DoesNotSkipAccounts(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b", "account-c", "account-d")
	first, err := s.resolveAccount("")
	if err != nil || first.ID != "account-a" {
		t.Fatalf("initial account=%q err=%v", first.ID, err)
	}

	confirmed429 := &chathub.RateLimitError{StatusCode: http.StatusTooManyRequests, Reason: "rate limited"}
	s.markAccountResult(first.ID, confirmed429)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(staleSuccess bool) {
			defer wg.Done()
			if staleSuccess {
				s.markAccountResult(first.ID, nil)
				return
			}
			s.markAccountResult(first.ID, confirmed429)
		}(i%2 == 0)
	}
	wg.Wait()

	if activeID := s.currentActiveAccountID(); activeID != "account-b" {
		t.Fatalf("concurrent stale results advanced active account to %s; want account-b", activeID)
	}
	if s.healthPool().Available(first.ID) {
		t.Fatal("stale success cleared the active 429 cooldown")
	}
	for i := 0; i < 20; i++ {
		account, err := s.resolveAccount("")
		if err != nil {
			t.Fatalf("resolve after concurrent 429 %d: %v", i, err)
		}
		if account.ID != "account-b" {
			t.Fatalf("request %d used %s; concurrent 429 must advance only to account-b", i, account.ID)
		}
	}
}

func TestRetriedAccountFailureIsQuarantinedWithoutSecondAdvance(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b", "account-c", "account-d")
	first, err := s.resolveAccount("")
	if err != nil || first.ID != "account-a" {
		t.Fatalf("initial account=%q err=%v", first.ID, err)
	}

	failure := errors.New("HTTP 503 service unavailable")
	s.markAccountResult(first.ID, failure)
	if active := s.currentActiveAccountID(); active != "account-b" {
		t.Fatalf("first failure advanced to %s; want account-b", active)
	}

	// This is the retry attempt inside the same client request. It must be
	// quarantined, but must not consume account-c until another request starts.
	s.recordAccountFailureWithoutAdvance("account-b", failure)
	if active := s.activeAccountID; active != "account-b" {
		t.Fatalf("retry failure advanced twice in one request to %s; want account-b", active)
	}
	if s.healthPool().Available("account-b") {
		t.Fatal("failed retry account was not quarantined")
	}

	// Two distinct identities failed with an infrastructure-shaped error, so
	// independent requests are held briefly instead of draining account-c.
	if _, err := s.resolveActiveAccount(); !errors.Is(err, errUpstreamCircuitOpen) {
		t.Fatalf("next request preflight err=%v; want shared upstream circuit", err)
	}
	if active := s.currentActiveAccountID(); active != "account-b" {
		t.Fatalf("open circuit advanced to %s; want account-b held", active)
	}
	s.accountRouteMu.Lock()
	s.upstreamCircuitUntil = time.Now().Add(-time.Second)
	s.accountRouteMu.Unlock()
	if _, err := s.resolveActiveAccount(); !errors.Is(err, errPinnedAccountUnavailable) {
		t.Fatalf("post-rest preflight err=%v; want pinned-account unavailable", err)
	}
	if active := s.currentActiveAccountID(); active != "account-c" {
		t.Fatalf("post-rest request advanced to %s; want account-c", active)
	}
}

func TestAccountSwitchDrainsExistingLeaseAndIsolatesNewCalls(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b")
	if _, err := s.resolveAccount(""); err != nil {
		t.Fatal(err)
	}
	leaseCtx, release, err := s.beginActiveAccountCall(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	s.markAccountResult("account-a", errors.New("HTTP 429 throttled"))
	select {
	case <-leaseCtx.Done():
		t.Fatal("account switch canceled an unrelated in-flight long task")
	default:
	}
	if _, _, err := s.beginActiveAccountCall(context.Background(), "account-a"); !errors.Is(err, errInactiveAccount) {
		t.Fatalf("retired account accepted a new call: %v", err)
	}
	_, releaseNext, err := s.beginActiveAccountCall(context.Background(), "account-b")
	if err != nil {
		t.Fatalf("new active account rejected a call: %v", err)
	}
	releaseNext()
}
