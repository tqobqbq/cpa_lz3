package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRequestSourceUsesAuthenticatedKeyAndResolvedIP(t *testing.T) {
	ctx := coreusage.WithAPIKey(context.Background(), "sk-client-original")
	ctx = logging.WithClientRequestMetadata(ctx, logging.ClientRequestMetadata{
		ClientIP: "192.0.2.1", ResolvedClientIP: "203.0.113.8", XForwardedFor: "untrusted-header", UserAgent: "client-original/1.0",
	})
	var before, after pluginapi.RequestInterceptRequest
	var completed pluginapi.RequestCompletion
	handler := &BaseAPIHandler{}
	handler.SetPluginHost(&handlerInterceptorTestHost{
		interceptRequestBeforeAuth: func(_ context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
			before = req
			return pluginapi.RequestInterceptResponse{Headers: http.Header{"Authorization": {"Bearer upstream-secret"}, "User-Agent": {"upstream-rewritten/2.0"}}}
		},
		interceptRequestAfterAuth: func(_ context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
			after = req
			return pluginapi.RequestInterceptResponse{}
		},
		completeRequest: func(_ context.Context, req pluginapi.RequestCompletion) { completed = req },
	})
	lifecycle := handler.newRequestLifecycleTracker(ctx, "openai", "model", "model", false, nil, "")
	_, opts, errMsg := handler.applyRequestInterceptorsBeforeAuth(ctx, "openai", "model", lifecycle.requestID(), coreexecutor.Request{Model: "model"}, coreexecutor.Options{Headers: http.Header{"Authorization": {"Bearer unrelated-header"}}}, "")
	if errMsg != nil {
		t.Fatal(errMsg)
	}
	handler.applyRequestInterceptorsAfterAuth(ctx, coreexecutor.RequestAfterAuthInterceptRequest{Headers: opts.Headers}, lifecycle.requestID(), "")
	lifecycle.complete(pluginapi.RequestCompletionCanceled, 0, context.Canceled)
	for _, req := range []pluginapi.RequestInterceptRequest{before, after} {
		if req.APIKey != "sk-client-original" || req.ClientIP != "203.0.113.8" || req.UserAgent != "client-original/1.0" {
			t.Fatalf("source changed with upstream headers: %+v", req)
		}
	}
	if completed.APIKey != before.APIKey || completed.ClientIP != before.ClientIP || completed.UserAgent != before.UserAgent {
		t.Fatalf("completion lost source identity: %+v", completed)
	}
}

func TestRequestSourceFallsBackToPeerWithoutTrustingForwardingHeader(t *testing.T) {
	ctx := logging.WithClientRequestMetadata(context.Background(), logging.ClientRequestMetadata{ClientIP: "2001:db8::1", XForwardedFor: "203.0.113.99"})
	key, ip, userAgent := requestSource(ctx)
	if key != "" || ip != "2001:db8::1" || userAgent != "" {
		t.Fatalf("unexpected source: key=%q ip=%q userAgent=%q", key, ip, userAgent)
	}
}
