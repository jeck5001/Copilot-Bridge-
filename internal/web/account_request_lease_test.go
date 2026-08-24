package web

import (
	"context"
	"errors"
	"testing"
)

func TestSharedOutageCircuitStopsAccountPoolDrain(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b", "account-c", "account-d")
	if !s.markAccountResult("account-a", errors.New("ws read before completion: read tcp: i/o timeout")) {
		t.Fatal("first independent failure should move to account-b")
	}
	s.recordAccountFailureWithoutAdvance("account-b", errors.New("HTTP 503 service unavailable"))
	if active := s.currentActiveAccountID(); active != "account-b" {
		t.Fatalf("shared outage drained route to %s; want account-b", active)
	}
	if _, err := s.resolveAccount(""); !errors.Is(err, errUpstreamCircuitOpen) {
		t.Fatalf("resolve during shared outage err=%v want %v", err, errUpstreamCircuitOpen)
	}
}

func TestIndependentProxyFailuresRemainSequential(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b", "account-c")
	if !s.markAccountResult("account-a", errors.New("proxy dialer: connection refused")) {
		t.Fatal("account-a proxy failure did not advance")
	}
	if !s.markAccountResult("account-b", errors.New("proxy dialer: connection refused")) {
		t.Fatal("account-b proxy failure was incorrectly treated as a shared outage")
	}
	if active := s.currentActiveAccountID(); active != "account-c" {
		t.Fatalf("active account=%s want account-c", active)
	}
}

func TestLogicalRequestLeaseSurvivesConcurrentRouteAdvance(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b")
	leaseCtx, releaseRequest, err := s.beginActiveAccountRequest(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRequest()

	if !s.markAccountResult("account-a", errors.New("HTTP 503 service unavailable")) {
		t.Fatal("expected account-a failure to advance the global route")
	}
	if active := s.currentActiveAccountID(); active != "account-b" {
		t.Fatalf("active account=%s want account-b", active)
	}

	// A second subcall belonging to the already-running logical turn may drain
	// on account-a. This is what keeps router -> repair -> answer workflows from
	// breaking because another request advanced the global pointer.
	callCtx, releaseCall, err := s.beginActiveAccountCall(leaseCtx, "account-a")
	if err != nil {
		t.Fatalf("leased follow-up subcall was rejected: %v", err)
	}
	releaseCall()
	if callCtx.Err() != nil {
		t.Fatalf("leased follow-up context unexpectedly canceled: %v", callCtx.Err())
	}

	// A new independent request cannot contact the isolated identity.
	if _, _, err := s.beginActiveAccountCall(context.Background(), "account-a"); !errors.Is(err, errInactiveAccount) {
		t.Fatalf("new call to isolated account err=%v want %v", err, errInactiveAccount)
	}
}

func TestLogicalRequestLeaseRejectsDifferentIdentity(t *testing.T) {
	s := newStickyAccountTestServer(t, "account-a", "account-b")
	leaseCtx, releaseRequest, err := s.beginActiveAccountRequest(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRequest()

	if _, _, err := s.beginActiveAccountCall(leaseCtx, "account-b"); !errors.Is(err, errInactiveAccount) {
		t.Fatalf("lease crossed identity boundary: err=%v", err)
	}
}
