package main

import (
	_ "embed"
	"encoding/json"
	"math"
	"sort"
	"strings"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

//go:embed prices.json
var priceCatalogJSON []byte

//go:embed service_prices.json
var servicePriceCatalogJSON []byte

// Prices are text API reference rates, not subscription or reseller bills.
type tokenPrice struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
	Reasoning  *float64 `json:"reasoning,omitempty"`
}

type modelCost struct {
	tokenPrice
	Tiers []struct {
		tokenPrice
		Tier struct {
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tier"`
	} `json:"tiers"`
	Over200K *tokenPrice `json:"context_over_200k,omitempty"`
}

type catalogPrice struct {
	Cost           modelCost            `json:"cost"`
	ServiceTiers   map[string]modelCost `json:"service_tiers,omitempty"`
	Source         string               `json:"source,omitempty"`
	CanonicalModel string               `json:"canonical_model_id,omitempty"`
}

type priceCatalog struct {
	Source    string                             `json:"source"`
	FetchedAt string                             `json:"fetched_at"`
	Providers map[string]map[string]catalogPrice `json:"providers"`
}

var officialTierPrices = func() priceCatalog {
	var catalog priceCatalog
	_ = json.Unmarshal(servicePriceCatalogJSON, &catalog)
	return catalog
}()

var officialPrices = func() priceCatalog {
	var catalog priceCatalog
	// The embedded snapshot is validated by tests. Invalid data leaves prices unknown.
	_ = json.Unmarshal(priceCatalogJSON, &catalog)
	if catalog.Providers == nil {
		catalog.Providers = make(map[string]map[string]catalogPrice)
	}
	for provider, models := range officialTierPrices.Providers {
		if catalog.Providers[provider] == nil {
			catalog.Providers[provider] = make(map[string]catalogPrice)
		}
		for model, entry := range models {
			entry.Source = officialTierPrices.Source
			catalog.Providers[provider][model] = entry
		}
	}
	return catalog
}()

type priceQuote struct {
	Provider    string      `json:"provider,omitempty"`
	Model       string      `json:"model,omitempty"`
	Rates       *tokenPrice `json:"rates,omitempty"`
	Above       int64       `json:"context_above,omitempty"`
	CostUSD     *float64    `json:"cost_usd"`
	Reason      string      `json:"reason,omitempty"`
	ServiceTier string      `json:"service_tier,omitempty"`
	TierSource  string      `json:"tier_source,omitempty"`
	Source      string      `json:"source,omitempty"`
}

type reportCost struct {
	USD             float64          `json:"usd"`
	Priced          int64            `json:"priced"`
	Missing         int64            `json:"missing"`
	MissingReasons  map[string]int64 `json:"missing_reasons,omitempty"`
	TierUnconfirmed int64            `json:"tier_unconfirmed,omitempty"`
}

func (c *reportCost) add(other reportCost) {
	c.USD += other.USD
	c.Priced += other.Priced
	c.Missing += other.Missing
	c.TierUnconfirmed += other.TierUnconfirmed
	if len(other.MissingReasons) > 0 {
		if c.MissingReasons == nil {
			c.MissingReasons = make(map[string]int64)
		}
		for reason, count := range other.MissingReasons {
			c.MissingReasons[reason] += count
		}
	}
}

func (q priceQuote) summary() reportCost {
	if q.CostUSD == nil {
		reason := q.Reason
		if reason == "" {
			reason = "unknown"
		}
		return reportCost{Missing: 1, MissingReasons: map[string]int64{reason: 1}}
	}
	result := reportCost{USD: *q.CostUSD, Priced: 1}
	if q.TierSource != "" && q.TierSource != "response" {
		result.TierUnconfirmed = 1
	}
	return result
}

func officialProvider(provider string) string {
	switch strings.ToLower(provider) {
	case "codex", "openai", "openai-compatibility":
		return "openai"
	case "claude", "anthropic":
		return "anthropic"
	case "gemini", "antigravity", "aistudio", "vertex", "interactions":
		return "google"
	case "grok":
		return "xai"
	case "kimi":
		return "moonshotai"
	case "qwen":
		return "alibaba"
	}
	return strings.ToLower(provider)
}

func quotePrice(provider, model string, tokens coreusage.TokenBreakdown, observed bool) priceQuote {
	return quoteTierPrice(provider, model, "default", tokens, observed)
}

func normalizedServiceTier(tier string) string {
	switch value := strings.ToLower(strings.TrimSpace(tier)); value {
	case "standard", "default":
		return "default"
	case "priority", "fast":
		return "fast"
	case "ultra-fast", "ultra_fast", "ultrafast":
		return "ultrafast"
	default:
		return value
	}
}

func quoteExecutionPrice(event reportExecution, tokens coreusage.TokenBreakdown) priceQuote {
	tier, source := normalizedServiceTier(event.ResponseServiceTier), "response"
	// Codex OAuth returns default/auto even when Fast is active. Legacy Codex
	// records without an auth kind are also treated as unconfirmed, not downgraded.
	codexUnconfirmed := codexResponseTierUnconfirmed(event.Provider, event.AuthType, event.ResponseServiceTier)
	if tier == "" || tier == "auto" || codexUnconfirmed {
		if outbound := strings.TrimSpace(event.OutboundServiceTier); outbound != "" {
			tier, source = normalizedServiceTier(outbound), "outbound"
		} else {
			// Legacy records do not contain the final outbound payload tier.
			tier, source = normalizedServiceTier(event.ServiceTier), "request"
			if codexUnconfirmed {
				source = "codex_request"
			}
		}
	}
	if tier == "" || tier == "auto" {
		tier, source = "default", "standard_reference"
	}
	quote := quoteTierPrice(event.Provider, event.Model, tier, tokens, tokens.TotalTokens > 0)
	quote.TierSource = source
	return quote
}

func codexResponseTierUnconfirmed(provider, authType, responseTier string) bool {
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(authType)) {
	case "apikey", "api_key", "api-key":
		return false
	}
	tier := normalizedServiceTier(responseTier)
	return tier == "" || tier == "auto" || tier == "default"
}

func quoteTierPrice(provider, model, serviceTier string, tokens coreusage.TokenBreakdown, observed bool) priceQuote {
	model = strings.ToLower(modelName(model))
	provider = officialProvider(provider)
	if prefix, name, ok := strings.Cut(model, "/"); ok && officialPrices.Providers[officialProvider(prefix)] != nil {
		provider, model = officialProvider(prefix), name
	}
	entry, found := officialPrices.Providers[provider][model]
	if !found {
		// Only accept an exact, unique official model ID; never guess a model family.
		matches := 0
		for owner, models := range officialPrices.Providers {
			if candidate, ok := models[model]; ok {
				provider, entry, matches = owner, candidate, matches+1
			}
		}
		found = matches == 1
	}
	quote := priceQuote{Model: model, Provider: provider, ServiceTier: serviceTier, Reason: "model_not_priced"}
	if !found {
		return quote
	}
	quote.Source = entry.Source
	if quote.Source == "" {
		quote.Source = officialPrices.Source
	}
	costTable := entry.Cost
	if serviceTier != "default" {
		var ok bool
		costTable, ok = entry.ServiceTiers[serviceTier]
		if !ok {
			quote.Reason = "service_tier_not_priced"
			return quote
		}
	}
	rates, above := costTable.tokenPrice, int64(0)
	for _, tier := range costTable.Tiers {
		if tier.Tier.Type == "context" && tokens.Input.TotalTokens > tier.Tier.Size && tier.Tier.Size > above {
			rates, above = tier.tokenPrice, tier.Tier.Size
		}
	}
	if len(costTable.Tiers) == 0 && tokens.Input.TotalTokens > 200000 && costTable.Over200K != nil {
		rates, above = *costTable.Over200K, 200000
	}
	quote.Rates, quote.Above = &rates, above
	if !observed {
		quote.Reason = "usage_missing"
		return quote
	}
	if !tokens.Valid() || tokens.Quality != coreusage.TokenAccountingQualityComplete {
		quote.Reason = "token_accounting_unknown"
		return quote
	}
	reasoningRate := rates.Reasoning
	if reasoningRate == nil {
		reasoningRate = rates.Output
	}
	cost := float64(0)
	for _, part := range []struct {
		tokens int64
		rate   *float64
	}{
		{tokens.Input.UncachedTokens, rates.Input}, {tokens.Input.CacheReadTokens, rates.CacheRead},
		{tokens.Input.CacheWriteTokens, rates.CacheWrite}, {tokens.Output.NonReasoningTokens, rates.Output},
		{tokens.Output.ReasoningTokens, reasoningRate},
	} {
		if part.tokens == 0 {
			continue
		}
		if part.rate == nil || *part.rate < 0 || math.IsNaN(*part.rate) || math.IsInf(*part.rate, 0) {
			quote.Reason = "rate_missing"
			return quote
		}
		cost += float64(part.tokens) * *part.rate / 1e6
	}
	quote.CostUSD, quote.Reason = &cost, ""
	return quote
}

func tokenAccounting(tokens pluginapi.UsageDetail, provider string) coreusage.TokenBreakdown {
	if tokens.TokenBreakdown.Valid() {
		return tokens.TokenBreakdown
	}
	cache := max(0, tokens.CacheReadTokens)
	if cache == 0 && tokens.CacheCreationTokens == 0 {
		cache = max(0, tokens.CachedTokens)
	}
	input, output, write, reasoning, total := max(0, tokens.InputTokens), max(0, tokens.OutputTokens), max(0, tokens.CacheCreationTokens), max(0, tokens.ReasoningTokens), max(0, tokens.TotalTokens)
	switch officialProvider(provider) {
	case "anthropic":
		// Legacy Claude output includes reasoning, while its input excludes cache.
		return coreusage.NewIndependentTokenBreakdown(input, cache, write, max(0, output-reasoning), reasoning, total)
	case "google":
		return coreusage.NewSeparateReasoningTokenBreakdown(input, cache, write, output, reasoning, total)
	case "openai", "xai", "moonshotai", "alibaba", "deepseek", "mistral", "zhipuai", "minimax":
		return coreusage.NewPartialSubsetTokenBreakdown(input, cache, write, output, reasoning, total)
	default:
		return coreusage.NewUnclassifiedTokenBreakdown(max(total, input+output))
	}
}

func appendPriceQuote(quotes []priceQuote, quote *priceQuote) []priceQuote {
	if quote == nil {
		return quotes
	}
	value := *quote
	value.CostUSD = nil
	if value.Rates != nil {
		value.Reason = ""
	}
	raw, _ := json.Marshal(value)
	for _, existing := range quotes {
		previous, _ := json.Marshal(existing)
		if string(previous) == string(raw) {
			return quotes
		}
	}
	return append(quotes, value)
}

func requestPriceQuotes(record reportRequest) []priceQuote {
	quotes := make([]priceQuote, 0, len(record.Executions))
	for _, execution := range record.Executions {
		quotes = appendPriceQuote(quotes, execution.Price)
	}
	sort.Slice(quotes, func(i, j int) bool {
		if quotes[i].Model == quotes[j].Model {
			return quotes[i].ServiceTier < quotes[j].ServiceTier
		}
		return quotes[i].Model < quotes[j].Model
	})
	return quotes
}

func priceRequest(record *reportRequest) {
	for i := range record.Executions {
		e := &record.Executions[i]
		tokens := tokenAccounting(e.Tokens, e.Provider)
		e.Tokens.TokenBreakdown = tokens
		quote := quoteExecutionPrice(*e, tokens)
		e.Price = &quote
	}
}
