package main

import (
	"encoding/json"
	"math"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func installTestPrices(t *testing.T) {
	t.Helper()
	previous := officialPrices
	t.Cleanup(func() { officialPrices = previous })
	raw := `{"source":"https://models.dev/api.json","fetched_at":"2026-11-06T00:00:00Z","providers":{"openai":{"test-model":{"cost":{"input":2,"output":10,"cache_read":0.2,"cache_write":3,"tiers":[{"input":4,"output":15,"cache_read":0.4,"cache_write":6,"tier":{"type":"context","size":1000}}]}},"free":{"cost":{"input":0,"output":0,"cache_read":0}},"no-cache-price":{"cost":{"input":2,"output":10}}}}}`
	if err := json.Unmarshal([]byte(raw), &officialPrices); err != nil {
		t.Fatal(err)
	}
}

func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-10 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBundledPricesAreValid(t *testing.T) {
	var catalog priceCatalog
	if err := json.Unmarshal(priceCatalogJSON, &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Source != "https://models.dev/api.json" || catalog.FetchedAt == "" || len(catalog.Providers) < 3 {
		t.Fatalf("incomplete price metadata: %+v", catalog)
	}
	for provider, models := range catalog.Providers {
		for model, price := range models {
			if price.Cost.Input == nil || price.Cost.Output == nil || *price.Cost.Input < 0 || *price.Cost.Output < 0 {
				t.Fatalf("invalid price %s/%s", provider, model)
			}
		}
	}
}

func TestOfficialPricingUsesExclusiveBucketsAndContextTier(t *testing.T) {
	installTestPrices(t)
	b := coreusage.NewSubsetTokenBreakdown(1000, 800, 100, 100, 40, 1100)
	q := quotePrice("codex", "test-model(high)", b, true)
	if q.CostUSD == nil || q.Above != 0 {
		t.Fatalf("quote = %+v", q)
	}
	near(t, *q.CostUSD, (100*2+800*.2+100*3+100*10)/1e6)
	b = coreusage.NewSubsetTokenBreakdown(1001, 800, 100, 100, 40, 1101)
	q = quotePrice("codex", "test-model", b, true)
	if q.CostUSD == nil || q.Above != 1000 {
		t.Fatalf("tier quote = %+v", q)
	}
	near(t, *q.CostUSD, (101*4+800*.4+100*6+100*15)/1e6)
}

func TestPricingDistinguishesUnknownRatesAndFreeUsage(t *testing.T) {
	installTestPrices(t)
	b := coreusage.NewSubsetTokenBreakdown(10, 5, 0, 2, 0, 12)
	for _, test := range []struct {
		model    string
		observed bool
		reason   string
	}{{"unknown", true, "model_not_priced"}, {"no-cache-price", true, "rate_missing"}, {"test-model", false, "usage_missing"}} {
		q := quotePrice("codex", test.model, b, test.observed)
		if q.CostUSD != nil || q.Reason != test.reason {
			t.Errorf("%s = %+v", test.model, q)
		}
	}
	free := quotePrice("codex", "free", b, true)
	if free.CostUSD == nil || *free.CostUSD != 0 {
		t.Fatalf("free = %+v", free)
	}
	invalid := quotePrice("codex", "test-model", coreusage.NewUnclassifiedTokenBreakdown(10), true)
	if invalid.CostUSD != nil || invalid.Reason != "token_accounting_unknown" {
		t.Fatalf("unclassified = %+v", invalid)
	}
}

func TestLegacyTokenSemanticsDoNotDoubleCountCacheOrReasoning(t *testing.T) {
	for _, test := range []struct {
		provider                string
		tokens                  pluginapi.UsageDetail
		input, output, uncached int64
	}{
		{"claude", pluginapi.UsageDetail{InputTokens: 10, CacheReadTokens: 80, CacheCreationTokens: 10, OutputTokens: 20, ReasoningTokens: 5, TotalTokens: 120}, 100, 20, 10},
		{"claude", pluginapi.UsageDetail{InputTokens: 90, CachedTokens: 10, CacheCreationTokens: 10, OutputTokens: 20, TotalTokens: 120}, 100, 20, 90},
		{"gemini", pluginapi.UsageDetail{InputTokens: 100, CacheReadTokens: 80, OutputTokens: 15, ReasoningTokens: 5, TotalTokens: 120}, 100, 20, 20},
		{"codex", pluginapi.UsageDetail{InputTokens: 100, CacheReadTokens: 80, OutputTokens: 20, ReasoningTokens: 5, TotalTokens: 120}, 100, 20, 20},
	} {
		got := tokenAccounting(test.tokens, test.provider)
		if !got.Valid() || got.Quality != coreusage.TokenAccountingQualityComplete || got.Input.TotalTokens != test.input || got.Output.TotalTokens != test.output || got.Input.UncachedTokens != test.uncached || got.TotalTokens != 120 {
			t.Errorf("%s: %+v", test.provider, got)
		}
	}
}

func trafficFixture() reportRequest {
	start := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	tokens := pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 80, TotalTokens: 120, TokenBreakdown: coreusage.NewSubsetTokenBreakdown(100, 80, 0, 20, 0, 120)}
	return reportRequest{ID: "request-1", APIKey: "sk-client-alice", ClientIP: "203.0.113.25", Provider: "codex", Model: "test-model", RequestedModel: "test-model(high)", AuthID: "credential-2", StartedAt: start, CompletedAt: start.Add(10 * time.Second), Outcome: "succeeded", Stream: true,
		Executions: []reportExecution{{Provider: "codex", AuthID: "credential-1", Model: "test-model", Tokens: tokens, Failed: true, LatencyMs: 1000}, {Provider: "openai", AuthID: "credential-2", Model: "test-model", Tokens: tokens, TTFTMs: 1000, LatencyMs: 5000}},
	}
}

func TestRequestTPSExcludesFailedAttemptsAndFirstTokenTime(t *testing.T) {
	installTestPrices(t)
	m := requestMetrics(trafficFixture())
	if m.Executions != 2 || m.Requests != 1 || m.Total != 240 || m.Cost.Priced != 2 {
		t.Fatalf("metrics = %+v", m)
	}
	near(t, m.TTFTMs, 1000)
	near(t, m.GenerationMs, 4000)
	near(t, m.EndToEndMs, 5000)
	near(t, float64(m.GenerationTokens)*1000/m.GenerationMs, 5)
	near(t, float64(m.EndToEndTokens)*1000/m.EndToEndMs, 4)
}

func TestBothTPSModesUseWeightedDurationsAndPreserveUnknownTTFT(t *testing.T) {
	first := reportTrafficMetrics{Output: 20}
	first.performance(5000, 1000, true)
	second := reportTrafficMetrics{Output: 60}
	second.performance(15000, 3000, true)
	first.add(second)
	near(t, float64(first.GenerationTokens)*1000/first.GenerationMs, 5)
	near(t, float64(first.EndToEndTokens)*1000/first.EndToEndMs, 4)
	if first.TPSCount != 2 || first.EndToEndCount != 2 {
		t.Fatalf("eligible samples: %+v", first)
	}
	missingTTFT := reportTrafficMetrics{Output: 10}
	missingTTFT.performance(2000, 0, true)
	if missingTTFT.TPSCount != 0 || missingTTFT.EndToEndCount != 1 {
		t.Fatalf("missing TTFT was fabricated or full duration dropped: %+v", missingTTFT)
	}
	for _, sample := range []struct{ succeeded, unknown bool }{{false, false}, {true, true}} {
		m := reportTrafficMetrics{Output: 10}
		if sample.unknown {
			m.AccountingUnknown = 1
		}
		m.performance(2000, 1000, sample.succeeded)
		if m.TPSCount != 0 || m.EndToEndCount != 0 {
			t.Fatalf("ineligible sample entered TPS: %+v", m)
		}
	}
}

func TestCostPendingReasonsSurviveRequestAndGroupAggregation(t *testing.T) {
	installTestPrices(t)
	r := trafficFixture()
	base := r.Executions[1]
	r.Executions = []reportExecution{base, base, base, base, base}
	r.Executions[1].Model = "unknown-model"
	r.Executions[2].Tokens = pluginapi.UsageDetail{}
	r.Executions[2].Failed = true
	r.Executions[3].Model = "no-cache-price"
	r.Executions[4].Tokens = pluginapi.UsageDetail{TokenBreakdown: coreusage.NewUnclassifiedTokenBreakdown(120)}
	priceRequest(&r)
	m := requestMetrics(r)
	if m.Cost.Priced != 1 || m.Cost.Missing != 4 || len(m.Cost.MissingReasons) != 4 {
		t.Fatalf("cost completeness: %+v", m.Cost)
	}
	for _, reason := range []string{"model_not_priced", "usage_missing", "rate_missing", "token_accounting_unknown"} {
		if m.Cost.MissingReasons[reason] != 1 {
			t.Fatalf("reason %q missing: %+v", reason, m.Cost)
		}
	}
	models, providers, credentials, keys := map[string]*reportGroup{}, map[string]*reportGroup{}, map[string]*reportGroup{}, map[string]*reportGroup{}
	addRequestGroups(r, models, providers, credentials, keys)
	addRequestGroups(r, models, providers, credentials, keys)
	client := keys[r.APIKey]
	if client.Cost.Missing != 8 || client.Cost.MissingReasons["usage_missing"] != 2 || client.Cost.Priced != 2 {
		t.Fatalf("client reason aggregation: %+v", client.Cost)
	}
	near(t, client.Cost.USD, m.Cost.USD*2)
	if m.Cost.MissingReasons["usage_missing"] != 1 {
		t.Fatal("group aggregation mutated the request's reason counts")
	}
}

func TestReportGroupsClientsSeparatelyFromProvidersAndCredentials(t *testing.T) {
	installTestPrices(t)
	store := newReportStore()
	store.configure(true, filepath.Join(t.TempDir(), "usage.sqlite"))
	t.Cleanup(store.close)
	r := trafficFixture()
	r.Selections = []reportSelection{{AuthID: "credential-1", Provider: "wire-format", Model: "test-model", ActualMs: 100}, {AuthID: "credential-2", Provider: "wire-format", Model: "test-model", ActualMs: 200}}
	store.change(r.ID, r.StartedAt, func(record *reportRequest) { *record = r })
	second := trafficFixture()
	second.ID, second.APIKey, second.ClientIP = "request-2", "sk-client-bob", "2001:db8::2"
	second.Executions = second.Executions[1:]
	store.change(second.ID, second.StartedAt, func(record *reportRequest) { *record = second })
	result, status, err := store.query(url.Values{"start": {"2026-11-06"}, "end": {"2026-11-06"}, "page_size": {"1"}}, r.CompletedAt)
	if err != nil || status != 200 || result.Total != 2 || len(result.Events) != 1 {
		t.Fatalf("query = %+v, %d, %v", result, status, err)
	}
	if result.Summary.Requests != 2 || result.Summary.Executions != 3 || result.Summary.Total != 360 || result.Events[0].APIKey != second.APIKey || result.Events[0].ClientIP != second.ClientIP || result.Events[0].Provider != "openai" {
		t.Fatalf("client source or totals missing: %+v", result)
	}
	if len(result.Keys) != 2 || len(result.Providers) != 2 || len(result.Credentials) != 2 {
		t.Fatalf("unexpected grouping: keys=%+v providers=%+v credentials=%+v", result.Keys, result.Providers, result.Credentials)
	}
	for _, client := range result.Keys {
		wantTotal, wantExecutions := int64(120), int64(1)
		if client.Name == r.APIKey {
			wantTotal, wantExecutions = 240, 2
		}
		if client.Requests != 1 || client.Total != wantTotal || client.Executions != wantExecutions || len(client.Prices) == 0 {
			t.Fatalf("client retries counted incorrectly: %+v", client)
		}
	}
	for _, provider := range result.Providers {
		want := int64(1)
		wait := float64(100)
		if provider.Name == "openai" {
			want = 2
			wait = 200
		}
		if provider.Executions != want || provider.Requests != want || provider.QuotaMs != wait {
			t.Fatalf("provider = %+v", provider)
		}
	}
	for _, term := range []string{r.APIKey, r.ClientIP} {
		filtered, _, err := store.query(url.Values{"start": {"2026-11-06"}, "end": {"2026-11-06"}, "q": {term}}, r.CompletedAt)
		if err != nil || filtered.Total != 1 || filtered.Events[0].APIKey != r.APIKey {
			t.Fatalf("source search failed for %q: %+v, %v", term, filtered, err)
		}
	}
}

func TestReportPersistsPlaintextSourceBeforeExecutionAndAcrossCallbacks(t *testing.T) {
	store := newReportStore()
	store.configure(true, filepath.Join(t.TempDir(), "usage.sqlite"))
	t.Cleanup(store.close)
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	c := &control{report: store, now: func() time.Time { return at }}
	key, ip, userAgent := "sk-client-<literal>&", "203.0.113.7", "test-client/1.0 <literal>&"
	c.reportBegin(interceptCall{RequestInterceptRequest: pluginapi.RequestInterceptRequest{RequestID: "source", APIKey: key, ClientIP: ip, UserAgent: userAgent, Model: "test-model"}})
	c.reportComplete(pluginapi.RequestCompletion{RequestID: "source", APIKey: key, ClientIP: ip, Outcome: pluginapi.RequestCompletionCanceled, StartedAt: at, CompletedAt: at.Add(time.Second)})
	response := c.usageRequest(url.Values{"id": {"source"}})
	var detail struct {
		Request reportRequest `json:"request"`
		Summary reportMetrics `json:"summary"`
	}
	if err := json.Unmarshal(response.Body, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Request.APIKey != key || detail.Request.ClientIP != ip || detail.Request.UserAgent != userAgent || detail.Request.Key != "" || detail.Summary.Executions != 0 {
		t.Fatalf("missing pre-execution source: %+v", detail)
	}
	c.reportUsage(pluginapi.UsageRecord{RequestID: "execution-1", LifecycleRequestID: "source", APIKey: "", ClientIP: "", Provider: "codex", RequestedAt: at})
	response = c.usageRequest(url.Values{"id": {"source"}})
	if err := json.Unmarshal(response.Body, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Request.APIKey != key || detail.Request.ClientIP != ip || detail.Request.UserAgent != userAgent {
		t.Fatalf("late usage erased identity: %+v", detail.Request)
	}
	c.reportUsage(pluginapi.UsageRecord{RequestID: "orphan", APIKey: key, ClientIP: ip, UserAgent: userAgent, Provider: "codex", RequestedAt: at})
	response = c.usageRequest(url.Values{"id": {"execution:orphan"}})
	if err := json.Unmarshal(response.Body, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Request.APIKey != key || detail.Request.ClientIP != ip || detail.Request.UserAgent != userAgent {
		t.Fatalf("orphan source missing: %+v", detail.Request)
	}
}

func TestReportUserAgentFilterAppliesToAllStatistics(t *testing.T) {
	installTestPrices(t)
	store := newReportStore()
	store.configure(true, filepath.Join(t.TempDir(), "usage.sqlite"))
	t.Cleanup(store.close)
	fixture := trafficFixture()
	for i, userAgent := range []string{"Codex_CLI/1.0", "Codex_CLI/2.0", "curl/8.0", ""} {
		record := trafficFixture()
		record.ID = []string{"codex-first", "codex-second", "curl", "historical"}[i]
		record.UserAgent = userAgent
		store.change(record.ID, record.StartedAt, func(saved *reportRequest) { *saved = record })
	}
	q := url.Values{"start": {"2026-11-06"}, "end": {"2026-11-06"}, "user_agent": {"  CODEX_cli  "}, "page_size": {"1"}}
	result, status, err := store.query(q, fixture.CompletedAt)
	if err != nil || status != 200 || result.Total != 2 || len(result.Events) != 1 || result.Events[0].UserAgent != "Codex_CLI/2.0" {
		t.Fatalf("user-agent query failed: %+v, %d, %v", result, status, err)
	}
	if result.Summary.Requests != 2 || result.Summary.Executions != 4 || result.Summary.Total != 480 || result.Keys[0].Total != 480 || result.Models[0].Total != 480 || len(result.Buckets) != 1 || result.Buckets[0].Total != 480 {
		t.Fatalf("user-agent filter omitted from aggregates: %+v", result)
	}
	for _, group := range result.Providers {
		if group.Total != 240 || group.Requests != 2 {
			t.Fatalf("provider aggregate includes other clients: %+v", group)
		}
	}
	q.Set("page", "2")
	result, _, err = store.query(q, fixture.CompletedAt)
	if err != nil || len(result.Events) != 1 || result.Events[0].UserAgent != "Codex_CLI/1.0" {
		t.Fatalf("user-agent pagination failed: %+v, %v", result, err)
	}
	q.Set("page", "1")
	q.Set("q", "curl")
	result, _, err = store.query(q, fixture.CompletedAt)
	if err != nil || result.Total != 0 || result.Summary.Total != 0 {
		t.Fatalf("combined filters must intersect: %+v, %v", result, err)
	}
	q.Del("user_agent")
	result, _, err = store.query(q, fixture.CompletedAt)
	if err != nil || result.Total != 1 || result.Events[0].UserAgent != "curl/8.0" {
		t.Fatalf("general search must include user-agent: %+v, %v", result, err)
	}
	if reportMatches(reportRequest{}, url.Values{"user_agent": {"codex"}}) {
		t.Fatal("historical record without user-agent matched a client filter")
	}
}

func TestHistoricalAnonymousKeyIsNotPresentedAsPlaintext(t *testing.T) {
	r := reportRequest{Key: "key-0123456789"}
	if got := clientKeyLabel(r); got != "历史匿名标识（无法还原）：key-0123456789" {
		t.Fatalf("legacy label = %q", got)
	}
	setRequestSource(&r, "sk-client", "192.0.2.1", "")
	if r.Key != "" || clientKeyLabel(r) != "sk-client" {
		t.Fatalf("new identity = %+v", r)
	}
}

func TestReportPersistsExecutionEffortAndResponseModelAcrossOutOfOrderUsage(t *testing.T) {
	installTestPrices(t)
	store := newReportStore()
	store.configure(true, filepath.Join(t.TempDir(), "usage.sqlite"))
	t.Cleanup(store.close)
	r := trafficFixture()
	tokens := r.Executions[0].Tokens
	r.Executions = nil
	store.change(r.ID, r.StartedAt, func(record *reportRequest) { *record = r })
	c := &control{report: store, now: func() time.Time { return r.CompletedAt }}
	for _, event := range []struct {
		id, auth, response, effort string
		offset                     time.Duration
	}{
		{"final", r.AuthID, "upstream-final-revision", "xhigh", 2 * time.Second},
		{"earlier", r.AuthID, "upstream-earlier-revision", "low", time.Second},
		{"other-credential", "credential-1", "other-model", "medium", 3 * time.Second},
	} {
		c.reportUsage(pluginapi.UsageRecord{RequestID: event.id, LifecycleRequestID: r.ID, AuthID: event.auth,
			Provider: "codex", Model: "test-model", ResponseModel: event.response, ReasoningEffort: event.effort,
			RequestedAt: r.StartedAt.Add(event.offset), Detail: tokens})
	}
	result, status, err := store.query(url.Values{"start": {"2026-11-06"}, "end": {"2026-11-06"}}, r.CompletedAt)
	if err != nil || status != 200 || len(result.Events) != 1 {
		t.Fatalf("query: %+v, %d, %v", result, status, err)
	}
	row := result.Events[0]
	if row.ResponseModel != "upstream-final-revision" || row.ReasoningEffort != "xhigh" {
		t.Fatalf("row selected an earlier retry or other credential: %+v", row)
	}
	var detail struct {
		Request reportRequest `json:"request"`
	}
	response := c.usageRequest(url.Values{"id": {r.ID}})
	if err := json.Unmarshal(response.Body, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Request.ResponseModel != row.ResponseModel || detail.Request.ReasoningEffort != row.ReasoningEffort || len(detail.Request.Executions) != 3 {
		t.Fatalf("detail differs from row: %+v", detail.Request)
	}
	if detail.Request.Executions[1].ResponseModel != "upstream-earlier-revision" || detail.Request.Executions[1].ReasoningEffort != "low" {
		t.Fatalf("retry metadata was lost: %+v", detail.Request.Executions)
	}
	for _, term := range []string{"upstream-final-revision", "xhigh", "upstream-earlier-revision"} {
		if !reportMatches(detail.Request, url.Values{"q": {term}}) {
			t.Fatalf("metadata search did not match %q", term)
		}
	}
}

func TestReportDoesNotInferMissingEffortOrResponseFromModelNames(t *testing.T) {
	r := trafficFixture()
	r.ResponseModel, r.ReasoningEffort = "stale-response", "stale-effort"
	r.Executions[0].ResponseModel, r.Executions[0].ReasoningEffort = "earlier-response", "low"
	newReportStore().snapshot(&r, r.CompletedAt)
	if r.ResponseModel != "" || r.ReasoningEffort != "" {
		t.Fatalf("missing final metadata was inferred from request suffix or retry: %+v", r)
	}
}
