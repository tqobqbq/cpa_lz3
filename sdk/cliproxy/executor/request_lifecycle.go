package executor

import (
	"context"
	"strings"
)

// RequestLifecycleIDMetadataKey correlates admission, execution attempts and completion.
const RequestLifecycleIDMetadataKey = "request_lifecycle_id"

type requestLifecycleIDKey struct{}

// WithRequestLifecycleID preserves one request identity across credential retries.
func WithRequestLifecycleID(ctx context.Context, requestID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestLifecycleIDKey{}, strings.TrimSpace(requestID))
}

// RequestLifecycleIDFromContext returns the request identity shared by its attempts.
func RequestLifecycleIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	requestID, _ := ctx.Value(requestLifecycleIDKey{}).(string)
	return requestID
}
