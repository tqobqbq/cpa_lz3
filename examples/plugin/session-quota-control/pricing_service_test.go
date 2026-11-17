package main

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestServiceTierPricingUsesResponseBeforeRequest(t *testing.T) {
	tokens := coreusage.NewSubsetTokenBreakdown(100, 80, 0, 20, 5, 120)
	for _, test := range []struct {
		name, request, response, tier, source string
		multiplier                            float64
	}{
		{"standard", "default", "default", "default", "response", 1},
		{"fast alias", "fast", "priority", "fast", "response", 2},
		{"ultrafast", "ultrafast", "ultrafast", "ultrafast", "response", 6},
		{"downgraded", "priority", "default", "default", "response", 1},
		{"upgraded", "default", "ultrafast", "ultrafast", "response", 6},
		{"request estimate", "ultra-fast", "", "ultrafast", "request", 6},
		{"historical reference", "", "", "default", "standard_reference", 1},
		{"auto unresolved", "auto", "", "default", "standard_reference", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			quote := quoteExecutionPrice(reportExecution{Provider: "openai", Model: "gpt-6.1-sol", ServiceTier: test.request, ResponseServiceTier: test.response}, tokens)
			if quote.CostUSD == nil || quote.ServiceTier != test.tier || quote.TierSource != test.source || quote.Source != officialTierPrices.Source {
				t.Fatalf("wrong tier or provenance: %+v", quote)
			}
			near(t, *quote.CostUSD, .000248*test.multiplier)
			wantUnconfirmed := int64(0)
			if test.source != "response" {
				wantUnconfirmed = 1
			}
			if quote.summary().TierUnconfirmed != wantUnconfirmed {
				t.Fatalf("unconfirmed tier not exposed: %+v", quote.summary())
			}
		})
	}
}

func TestCodexDefaultResponseDoesNotConfirmStandardPricing(t *testing.T) {
	tokens := coreusage.NewSubsetTokenBreakdown(100, 80, 0, 20, 5, 120)
	for _, test := range []struct {
		name, auth, request, response, tier, source string
		multiplier                                  float64
	}{
		{"oauth fast", "oauth", "priority", "default", "fast", "codex_request", 2},
		{"oauth ultrafast", "oauth", "ultrafast", "default", "ultrafast", "codex_request", 6},
		{"oauth auto response", "oauth", "fast", "auto", "fast", "codex_request", 2},
		{"oauth no response tier", "oauth", "fast", "", "fast", "codex_request", 2},
		{"legacy auth unknown", "", "priority", "default", "fast", "codex_request", 2},
		{"oauth standard request", "oauth", "default", "default", "default", "codex_request", 1},
		{"oauth auto request", "oauth", "auto", "default", "default", "standard_reference", 1},
		{"oauth request unknown", "oauth", "", "default", "default", "standard_reference", 1},
		{"explicit fast response", "oauth", "default", "priority", "fast", "response", 2},
		{"api key downgrade", "apikey", "priority", "default", "default", "response", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			quote := quoteExecutionPrice(reportExecution{Provider: "codex", AuthType: test.auth, Model: "gpt-6.1-sol", ServiceTier: test.request, ResponseServiceTier: test.response}, tokens)
			if quote.CostUSD == nil || quote.ServiceTier != test.tier || quote.TierSource != test.source {
				t.Fatalf("wrong Codex tier estimate: %+v", quote)
			}
			near(t, *quote.CostUSD, .000248*test.multiplier)
			if (quote.summary().TierUnconfirmed > 0) != (test.source != "response") {
				t.Fatalf("wrong confirmation state: %+v", quote.summary())
			}
		})
	}
}

func TestPricingUsesFinalOutboundTierAfterPayloadRules(t *testing.T) {
	tokens := coreusage.NewSubsetTokenBreakdown(100, 80, 0, 20, 5, 120)
	for _, tc := range []struct {
		name, outbound, response, tier, source string
		multiplier                             float64
	}{
		{"filtered", "auto", "default", "default", "standard_reference", 1},
		{"standard override", "default", "default", "default", "outbound", 1},
		{"fast override", "priority", "default", "fast", "outbound", 2},
		{"confirmed response", "auto", "ultrafast", "ultrafast", "response", 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quote := quoteExecutionPrice(reportExecution{Provider: "codex", AuthType: "oauth", Model: "gpt-6.1-sol",
				ServiceTier: "priority", OutboundServiceTier: tc.outbound, ResponseServiceTier: tc.response}, tokens)
			if quote.CostUSD == nil || quote.ServiceTier != tc.tier || quote.TierSource != tc.source {
				t.Fatalf("wrong outbound estimate: %+v", quote)
			}
			near(t, *quote.CostUSD, .000248*tc.multiplier)
		})
	}
}

func TestOutboundTiersPersistPerRetryAndReachRequestDetail(t *testing.T) {
	store := newReportStore()
	store.configure(true, filepath.Join(t.TempDir(), "usage.sqlite"))
	t.Cleanup(store.close)
	r := trafficFixture()
	r.Executions = nil
	store.change(r.ID, r.StartedAt, func(record *reportRequest) { *record = r })
	c := &control{report: store, now: func() time.Time { return r.CompletedAt }}
	for _, event := range []struct {
		id, tier string
		offset   time.Duration
	}{{"final", "auto", 2 * time.Second}, {"retry", "priority", time.Second}} {
		c.reportUsage(pluginapi.UsageRecord{RequestID: event.id, LifecycleRequestID: r.ID, AuthID: r.AuthID,
			Provider: "codex", AuthType: "oauth", Model: "gpt-6.1-sol", RequestedAt: r.StartedAt.Add(event.offset),
			ServiceTier: "priority", OutboundServiceTier: event.tier, ResponseServiceTier: "default"})
	}
	result, status, errQuery := store.query(url.Values{"start": {"2026-11-06"}, "end": {"2026-11-06"}}, r.CompletedAt)
	if errQuery != nil || status != 200 || len(result.Events) != 1 || result.Events[0].OutboundServiceTier != "auto" {
		t.Fatalf("final outbound tier lost in report: %+v, %d, %v", result, status, errQuery)
	}
	var detail struct {
		Request reportRequest `json:"request"`
	}
	if errDecode := json.Unmarshal(c.usageRequest(url.Values{"id": {r.ID}}).Body, &detail); errDecode != nil {
		t.Fatal(errDecode)
	}
	if detail.Request.OutboundServiceTier != "auto" || len(detail.Request.Executions) != 2 || detail.Request.Executions[1].OutboundServiceTier != "priority" {
		t.Fatalf("per-attempt outbound tiers lost: %+v", detail.Request)
	}
	if !reportMatches(detail.Request, url.Values{"q": {"auto"}}) {
		t.Fatal("outbound tier cannot be searched")
	}
}

func TestServiceTierPricesAreModelSpecificAndContextAware(t *testing.T) {
	tokens := coreusage.NewSubsetTokenBreakdown(1000, 400, 100, 100, 40, 1100)
	quote := quoteExecutionPrice(reportExecution{Provider: "codex", Model: "gpt-6.1-sol", ResponseServiceTier: "ultrafast"}, tokens)
	if quote.CostUSD == nil {
		t.Fatalf("ultrafast pricing missing: %+v", quote)
	}
	near(t, *quote.CostUSD, (500*12+400*.6+100*15+100*60)/1e6)
	for _, test := range []struct {
		model string
		ratio float64
	}{
		{"gpt-5.5", 2.5}, {"gpt-6.1-sol", 2}, {"gpt-4o", 1.7},
	} {
		noWrite := coreusage.NewSubsetTokenBreakdown(1000, 400, 0, 100, 40, 1100)
		standard := quotePrice("openai", test.model, noWrite, true)
		fast := quoteExecutionPrice(reportExecution{Provider: "openai", Model: test.model, ResponseServiceTier: "fast"}, noWrite)
		if standard.CostUSD == nil || fast.CostUSD == nil {
			t.Fatalf("model %s missing prices: %+v, %+v", test.model, standard, fast)
		}
		near(t, *fast.CostUSD, *standard.CostUSD*test.ratio)
	}
	for _, test := range []struct {
		input int64
		tier  string
		rate  float64
	}{
		{272000, "fast", 4}, {272001, "fast", 8}, {272001, "ultrafast", 24}, {272001, "flex", 2},
	} {
		quote := quoteExecutionPrice(reportExecution{Provider: "codex", Model: "gpt-6.1-sol", ResponseServiceTier: test.tier}, coreusage.NewSubsetTokenBreakdown(test.input, 0, 0, 0, 0, test.input))
		if quote.CostUSD == nil || quote.Rates == nil {
			t.Fatalf("context prices missing: %+v", quote)
		}
		near(t, *quote.CostUSD, float64(test.input)*test.rate/1e6)
	}
}

func TestServiceTierDoesNotInventMissingRates(t *testing.T) {
	for _, test := range []struct {
		model, response, reason string
		input                   int64
	}{
		{"gpt-5.4", "ultrafast", "service_tier_not_priced", 100},
		{"gpt-6.1-sol", "future-tier", "service_tier_not_priced", 100},
		{"gpt-5.4", "fast", "rate_missing", 272001},
	} {
		quote := quoteExecutionPrice(reportExecution{Provider: "codex", Model: test.model, ServiceTier: "fast", ResponseServiceTier: test.response}, coreusage.NewSubsetTokenBreakdown(test.input, 0, 0, 10, 0, test.input+10))
		if quote.CostUSD != nil || quote.Reason != test.reason || quote.TierSource != "response" {
			t.Fatalf("unsupported tier used standard or requested price: %+v", quote)
		}
	}
}

func TestServiceTiersPersistPerRetryAndAggregateAcrossReports(t *testing.T) {
	store := newReportStore()
	store.configure(true, filepath.Join(t.TempDir(), "usage.sqlite"))
	t.Cleanup(store.close)
	r := trafficFixture()
	r.Executions = nil
	store.change(r.ID, r.StartedAt, func(record *reportRequest) { *record = r })
	c := &control{report: store, now: func() time.Time { return r.CompletedAt }}
	tokens := pluginapi.UsageDetail{TokenBreakdown: coreusage.NewSubsetTokenBreakdown(100, 80, 0, 20, 5, 120)}
	// Callback delivery order must not overwrite the final attempt's reported tier.
	for _, event := range []struct {
		id, tier string
		offset   time.Duration
	}{
		{"final", "default", 2 * time.Second}, {"retry", "ultrafast", time.Second},
	} {
		c.reportUsage(pluginapi.UsageRecord{RequestID: event.id, LifecycleRequestID: r.ID, AuthID: r.AuthID,
			Provider: "codex", AuthType: "oauth", Model: "gpt-6.1-sol", RequestedAt: r.StartedAt.Add(event.offset),
			ServiceTier: "ultrafast", ResponseServiceTier: event.tier, Detail: tokens})
	}
	result, status, err := store.query(url.Values{"start": {"2026-11-06"}, "end": {"2026-11-06"}}, r.CompletedAt)
	if err != nil || status != 200 || len(result.Events) != 1 {
		t.Fatalf("query: %+v, %d, %v", result, status, err)
	}
	row := result.Events[0]
	if row.ServiceTier != "ultrafast" || row.ResponseServiceTier != "default" || row.AuthType != "oauth" || len(row.Prices) != 2 {
		t.Fatalf("final tier or separate retry prices lost: %+v", row)
	}
	want := .000248 * 12
	for _, cost := range []reportCost{row.Cost, result.Summary.Cost, result.Keys[0].Cost, result.Models[0].Cost, result.Providers[0].Cost, result.Credentials[0].Cost, result.Buckets[0].Cost} {
		near(t, cost.USD, want)
		if cost.Priced != 2 || cost.Missing != 0 || cost.TierUnconfirmed != 1 {
			t.Fatalf("tier totals: %+v", cost)
		}
	}
	var detail struct {
		Request reportRequest `json:"request"`
	}
	if err := json.Unmarshal(c.usageRequest(url.Values{"id": {r.ID}}).Body, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Request.ResponseServiceTier != "default" || detail.Request.AuthType != "oauth" || detail.Request.Executions[1].ResponseServiceTier != "ultrafast" || detail.Request.Executions[1].AuthType != "oauth" {
		t.Fatalf("detail tiers not persisted: %+v", detail.Request)
	}
	if !reportMatches(detail.Request, url.Values{"q": {"ultrafast"}}) {
		t.Fatal("service tier cannot be searched")
	}
	legacy := trafficFixture()
	legacy.ServiceTier, legacy.ResponseServiceTier = "stale-request", "stale-response"
	store.snapshot(&legacy, legacy.CompletedAt)
	if legacy.ServiceTier != "" || legacy.ResponseServiceTier != "" {
		t.Fatalf("historical tier invented: %+v", legacy)
	}
}
