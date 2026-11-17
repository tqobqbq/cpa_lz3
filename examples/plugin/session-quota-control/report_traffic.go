package main

import (
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type reportTrafficMetrics struct {
	Input             int64      `json:"input"`
	Output            int64      `json:"output"`
	Cache             int64      `json:"cache"`
	CacheCreation     int64      `json:"cache_creation"`
	Reasoning         int64      `json:"reasoning"`
	Total             int64      `json:"total"`
	Uncached          int64      `json:"uncached"`
	Unclassified      int64      `json:"unclassified"`
	AccountingUnknown int64      `json:"accounting_unknown"`
	TTFTMs            float64    `json:"ttft_ms"`
	TTFTCount         int64      `json:"ttft_count"`
	GenerationMs      float64    `json:"generation_ms"`
	GenerationTokens  int64      `json:"generation_tokens"`
	TPSCount          int64      `json:"tps_count"`
	EndToEndMs        float64    `json:"end_to_end_ms"`
	EndToEndTokens    int64      `json:"end_to_end_tokens"`
	EndToEndCount     int64      `json:"end_to_end_count"`
	Cost              reportCost `json:"cost"`
}

func (m *reportTrafficMetrics) add(other reportTrafficMetrics) {
	m.Input += other.Input
	m.Output += other.Output
	m.Cache += other.Cache
	m.CacheCreation += other.CacheCreation
	m.Reasoning += other.Reasoning
	m.Total += other.Total
	m.Uncached += other.Uncached
	m.Unclassified += other.Unclassified
	m.AccountingUnknown += other.AccountingUnknown
	m.TTFTMs += other.TTFTMs
	m.TTFTCount += other.TTFTCount
	m.GenerationMs += other.GenerationMs
	m.GenerationTokens += other.GenerationTokens
	m.TPSCount += other.TPSCount
	m.EndToEndMs += other.EndToEndMs
	m.EndToEndTokens += other.EndToEndTokens
	m.EndToEndCount += other.EndToEndCount
	m.Cost.add(other.Cost)
}

func trafficTokens(tokens coreusage.TokenBreakdown) reportTrafficMetrics {
	m := reportTrafficMetrics{Input: tokens.Input.TotalTokens, Output: tokens.Output.TotalTokens, Cache: tokens.Input.CacheReadTokens, CacheCreation: tokens.Input.CacheWriteTokens,
		Reasoning: tokens.Output.ReasoningTokens, Total: tokens.TotalTokens, Uncached: tokens.Input.UncachedTokens, Unclassified: tokens.UnclassifiedTokens}
	if !tokens.Valid() || tokens.Quality != coreusage.TokenAccountingQualityComplete {
		m.AccountingUnknown = 1
	}
	return m
}

func (m *reportTrafficMetrics) performance(duration, firstToken float64, succeeded bool) {
	if !succeeded {
		return
	}
	if duration > 0 && m.Output > 0 && m.AccountingUnknown == 0 {
		m.EndToEndMs, m.EndToEndTokens, m.EndToEndCount = duration, m.Output, 1
	}
	if firstToken > 0 && duration >= firstToken {
		m.TTFTMs, m.TTFTCount = firstToken, 1
		if duration > firstToken && m.Output > 0 && m.AccountingUnknown == 0 {
			m.GenerationMs, m.GenerationTokens, m.TPSCount = duration-firstToken, m.Output, 1
		}
	}
}
