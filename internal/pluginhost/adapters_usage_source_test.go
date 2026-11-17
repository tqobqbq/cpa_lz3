package pluginhost

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestUsageAdapterPreservesClientSourceAndCanonicalTokens(t *testing.T) {
	plugin := &capturingUsagePlugin{captured: make(chan pluginapi.UsageRecord, 1)}
	host := newHostWithRecords(capabilityRecord{id: "usage-source", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{UsagePlugin: plugin}}})
	adapter := &usageAdapter{host: host, pluginID: "usage-source"}
	ctx := coreusage.WithAPIKey(context.Background(), "sk-client-source")
	ctx = logging.WithClientRequestMetadata(ctx, logging.ClientRequestMetadata{ClientIP: "192.0.2.1", ResolvedClientIP: "203.0.113.9", UserAgent: "client-original/1.0"})
	tokens := coreusage.NewIndependentTokenBreakdown(10, 80, 10, 20, 0, 120)
	adapter.HandleUsage(ctx, coreusage.Record{Provider: "claude", Detail: coreusage.Detail{TokenBreakdown: tokens}})
	record := <-plugin.captured
	if record.APIKey != "sk-client-source" || record.ClientIP != "203.0.113.9" || record.UserAgent != "client-original/1.0" || record.Detail.TokenBreakdown != tokens {
		t.Fatalf("usage source or tokens not preserved: %+v", record)
	}
}
