package helps

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageReporterOutboundServiceTier(t *testing.T) {
	for _, tc := range []struct {
		name, payload, want string
	}{
		{"filtered", `{"model":"gpt-5.5"}`, "auto"},
		{"override", `{"service_tier":"default"}`, "default"},
		{"priority", `{"service_tier":"priority"}`, "priority"},
		{"claude fast", `{"speed":"fast"}`, "priority"},
		{"invalid", `not-json`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := usage.WithServiceTier(context.Background(), "priority")
			reporter := NewUsageReporter(ctx, "codex", "gpt-5.5", nil)
			reporter.SetOutboundServiceTier([]byte(tc.payload))
			record := reporter.buildRecord(usage.Detail{}, false)
			if record.OutboundServiceTier != tc.want || record.ServiceTier != "priority" {
				t.Fatalf("tiers = outbound %q, client %q; want %q, priority", record.OutboundServiceTier, record.ServiceTier, tc.want)
			}
		})
	}
}

func TestUsageReporterHTTPObservesFinalBodyWithoutConsumingIt(t *testing.T) {
	ctx := usage.WithServiceTier(context.Background(), "priority")
	reporter := NewUsageReporter(ctx, "codex", "gpt-5.5", nil)
	const payload = `{"model":"gpt-5.5","input":"hello"}`
	client := reporter.TrackHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		if string(body) != payload {
			t.Fatalf("transport body changed: %q", body)
		}
		if got := reporter.buildRecord(usage.Detail{}, false).OutboundServiceTier; got != "auto" {
			t.Fatalf("outbound tier before transport = %q, want auto", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	})})
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.test/responses", strings.NewReader(payload))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
}
