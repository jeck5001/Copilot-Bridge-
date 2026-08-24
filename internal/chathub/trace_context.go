package chathub

import (
	"context"
	"strings"
)

type correlationIDKey struct{}

// WithCorrelationID attaches the gateway request ID to all ChatHub subcalls in
// one logical operation. It is diagnostic metadata only and is never sent to
// Microsoft as an upstream request identifier.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	if ctx == nil || strings.TrimSpace(id) == "" {
		return ctx
	}
	return context.WithValue(ctx, correlationIDKey{}, strings.TrimSpace(id))
}

// CorrelationID returns the gateway request ID attached by the HTTP boundary.
func CorrelationID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(correlationIDKey{}).(string)
	return id
}
