package chathub

import (
	"context"
	"testing"
)

func TestCorrelationIDRoundTrip(t *testing.T) {
	ctx := WithCorrelationID(context.Background(), "  gateway-request-1  ")
	if got := CorrelationID(ctx); got != "gateway-request-1" {
		t.Fatalf("correlation id=%q", got)
	}
	if got := CorrelationID(context.Background()); got != "" {
		t.Fatalf("unexpected correlation id=%q", got)
	}
}
