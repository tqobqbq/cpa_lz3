package main

// Date ranges, breakdowns and token accounting follow CPA Usage Report
// v0.3.1. Request grouping and wait accounting extend that design for this plugin.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type reportMetrics struct {
	reportTrafficMetrics
	Requests         int64   `json:"requests"`
	Succeeded        int64   `json:"succeeded"`
	Failed           int64   `json:"failed"`
	Running          int64   `json:"running"`
	Executions       int64   `json:"executions"`
	FailedExecutions int64   `json:"failed_executions"`
	ManualMs         float64 `json:"manual_wait_ms"`
	QuotaPlannedMs   float64 `json:"quota_planned_ms"`
	QuotaMs          float64 `json:"quota_wait_ms"`
	UpstreamMs       float64 `json:"upstream_ms"`
	TotalMs          float64 `json:"total_ms"`
	DurationCount    int64   `json:"duration_count"`
	ManualUnknown    int64   `json:"manual_unknown"`
	QuotaUnknown     int64   `json:"quota_unknown"`
}

func (m *reportMetrics) add(other reportMetrics) {
	m.reportTrafficMetrics.add(other.reportTrafficMetrics)
	m.Requests += other.Requests
	m.Succeeded += other.Succeeded
	m.Failed += other.Failed
	m.Running += other.Running
	m.Executions += other.Executions
	m.FailedExecutions += other.FailedExecutions
	m.ManualMs += other.ManualMs
	m.QuotaPlannedMs += other.QuotaPlannedMs
	m.QuotaMs += other.QuotaMs
	m.UpstreamMs += other.UpstreamMs
	m.TotalMs += other.TotalMs
	m.DurationCount += other.DurationCount
	m.ManualUnknown += other.ManualUnknown
	m.QuotaUnknown += other.QuotaUnknown
}

func executionMetrics(event reportExecution) reportMetrics {
	tokens := tokenAccounting(event.Tokens, event.Provider)
	m := reportMetrics{Executions: 1, UpstreamMs: event.LatencyMs, reportTrafficMetrics: trafficTokens(tokens)}
	if event.Failed {
		m.FailedExecutions = 1
	}
	m.performance(event.LatencyMs, event.TTFTMs, !event.Failed)
	quote := event.Price
	if quote == nil {
		value := quoteExecutionPrice(event, tokens)
		quote = &value
	}
	m.Cost = quote.summary()
	return m
}

func elapsedMs(startedAt, finishedAt time.Time) float64 {
	if startedAt.IsZero() || finishedAt.IsZero() {
		return 0
	}
	return max(0, float64(finishedAt.Sub(startedAt))/float64(time.Millisecond))
}

func (s *reportStore) snapshot(record *reportRequest, now time.Time) {
	if record.Outcome == "running" && record.RunID != s.runID {
		record.Outcome = "interrupted"
	}
	// Usage records identify the actual provider, which may differ from the wire protocol.
	var final *reportExecution
	for i := range record.Executions {
		event := &record.Executions[i]
		if record.AuthID != "" && event.AuthID != record.AuthID {
			continue
		}
		if final == nil || !event.Timestamp.Before(final.Timestamp) {
			final = event
		}
	}
	if final != nil && final.Provider != "" {
		record.Provider = final.Provider
	}
	// Missing reported values must not be inferred from model names or earlier retries.
	record.ResponseModel, record.ReasoningEffort = "", ""
	record.ServiceTier, record.ResponseServiceTier = "", ""
	record.OutboundServiceTier = ""
	record.AuthType = ""
	if final != nil {
		record.ResponseModel, record.ReasoningEffort = final.ResponseModel, final.ReasoningEffort
		record.ServiceTier, record.ResponseServiceTier = final.ServiceTier, final.ResponseServiceTier
		record.OutboundServiceTier = final.OutboundServiceTier
		record.AuthType = final.AuthType
	}
	for i := range record.Admissions {
		admission := &record.Admissions[i]
		if admission.FinishedAt.IsZero() && record.Outcome == "running" {
			admission.ActualMs = elapsedMs(admission.StartedAt, now)
		}
	}
	for i := range record.Selections {
		selection := &record.Selections[i]
		if !selection.WaitStartedAt.IsZero() && selection.WaitFinishedAt.IsZero() && record.Outcome == "running" {
			selection.ActualMs = elapsedMs(selection.WaitStartedAt, now)
		}
	}
}

func requestMetrics(record reportRequest) reportMetrics {
	m := reportMetrics{Requests: 1}
	switch record.Outcome {
	case "running":
		m.Running = 1
	case "succeeded":
		m.Succeeded = 1
	default:
		m.Failed = 1
	}
	if !record.CompletedAt.IsZero() {
		m.TotalMs, m.DurationCount = elapsedMs(record.StartedAt, record.CompletedAt), 1
	}
	for _, admission := range record.Admissions {
		m.ManualMs += admission.ActualMs
		if admission.FinishedAt.IsZero() && record.Outcome != "running" {
			m.ManualUnknown++
		}
	}
	for _, selection := range record.Selections {
		m.QuotaMs += selection.ActualMs
		m.QuotaPlannedMs += selection.PlannedMs
		if !selection.WaitStartedAt.IsZero() && selection.WaitFinishedAt.IsZero() && record.Outcome != "running" {
			m.QuotaUnknown++
		}
	}
	for _, execution := range record.Executions {
		m.add(executionMetrics(execution))
	}
	return m
}

type reportRow struct {
	ID                  string       `json:"id"`
	TraceID             string       `json:"trace_id"`
	StartedAt           time.Time    `json:"started_at"`
	CompletedAt         time.Time    `json:"completed_at"`
	Model               string       `json:"model"`
	RequestedModel      string       `json:"requested_model"`
	ResponseModel       string       `json:"response_model,omitempty"`
	ReasoningEffort     string       `json:"reasoning_effort,omitempty"`
	ServiceTier         string       `json:"service_tier,omitempty"`
	OutboundServiceTier string       `json:"outbound_service_tier,omitempty"`
	ResponseServiceTier string       `json:"response_service_tier,omitempty"`
	Provider            string       `json:"provider"`
	APIKey              string       `json:"api_key,omitempty"`
	ClientIP            string       `json:"client_ip,omitempty"`
	UserAgent           string       `json:"user_agent,omitempty"`
	Key                 string       `json:"key,omitempty"`
	SessionID           string       `json:"session_id"`
	AuthID              string       `json:"auth_id"`
	AuthIndex           string       `json:"auth_index"`
	AuthType            string       `json:"auth_type,omitempty"`
	Stream              bool         `json:"stream"`
	Outcome             string       `json:"outcome"`
	StatusCode          int          `json:"status_code"`
	SelectionCount      int          `json:"selection_count"`
	Error               string       `json:"error,omitempty"`
	Prices              []priceQuote `json:"prices,omitempty"`
	reportMetrics
}

type reportGroup struct {
	Name   string       `json:"name"`
	Prices []priceQuote `json:"prices,omitempty"`
	reportMetrics
}

type reportBucket struct {
	Label     string    `json:"label"`
	StartedAt time.Time `json:"started_at"`
	reportMetrics
}

type reportResponse struct {
	Enabled          bool              `json:"enabled"`
	DBPath           string            `json:"db_path"`
	CollectErr       string            `json:"collect_err,omitempty"`
	CollectedAt      time.Time         `json:"collected_at"`
	LastWrite        time.Time         `json:"last_write"`
	Timezone         string            `json:"timezone"`
	ServerDate       string            `json:"server_date"`
	Skipped          int               `json:"skipped"`
	Total            int64             `json:"total"`
	Page             int               `json:"page"`
	PageSize         int               `json:"page_size"`
	Summary          reportMetrics     `json:"summary"`
	Events           []reportRow       `json:"events"`
	Buckets          []reportBucket    `json:"buckets"`
	Bucket           string            `json:"bucket"`
	Providers        []reportGroup     `json:"providers"`
	Models           []reportGroup     `json:"models"`
	Credentials      []reportGroup     `json:"credentials"`
	Keys             []reportGroup     `json:"keys"`
	ModelOptions     []string          `json:"model_options"`
	ProviderOptions  []string          `json:"provider_options"`
	CredentialLabels map[string]string `json:"credential_labels"`
	PriceSource      string            `json:"price_source"`
	PriceDate        string            `json:"price_date"`
	TierPriceSource  string            `json:"tier_price_source"`
	TierPriceDate    string            `json:"tier_price_date"`
}

func reportRange(q url.Values, now time.Time) (time.Time, time.Time, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	start, end := today.AddDate(0, 0, -6), today
	for key, dest := range map[string]*time.Time{"start": &start, "end": &end} {
		if value := q.Get(key); value != "" {
			parsed, errParse := time.ParseInLocation("2006-01-02", value, time.Local)
			if errParse != nil {
				return time.Time{}, time.Time{}, fmt.Errorf("invalid %s date", key)
			}
			*dest = parsed
		}
	}
	if start.After(end) {
		return time.Time{}, time.Time{}, fmt.Errorf("start date cannot be later than end date")
	}
	return start, end.AddDate(0, 0, 1), nil
}

func reportBucketStart(at time.Time, bucket string) time.Time {
	at = at.In(time.Local)
	if bucket == "hour" {
		// Preserve the UTC offset during the repeated hour at the end of DST.
		return at.Add(-time.Duration(at.Minute())*time.Minute - time.Duration(at.Second())*time.Second - time.Duration(at.Nanosecond()))
	}
	day := at.Day()
	if bucket == "month" {
		day = 1
	}
	return time.Date(at.Year(), at.Month(), day, 0, 0, 0, 0, at.Location())
}

func reportTimeline(start, end time.Time, bucket string) ([]reportBucket, error) {
	bins := []reportBucket{}
	if !start.Before(end) {
		return bins, nil
	}
	for at := reportBucketStart(start, bucket); at.Before(end); {
		if len(bins) >= 5000 {
			return nil, fmt.Errorf("too many trend intervals: shorten the date range or choose day/month granularity")
		}
		label := at.Format("2006-01-02")
		next := at.AddDate(0, 0, 1)
		if bucket == "hour" {
			label, next = at.Format("2006-01-02 15:04 -07:00"), at.Add(time.Hour)
		} else if bucket == "month" {
			label, next = at.Format("2006-01"), at.AddDate(0, 1, 0)
		}
		bins = append(bins, reportBucket{Label: label, StartedAt: at})
		at = next
	}
	return bins, nil
}

func reportMatches(record reportRequest, q url.Values) bool {
	if userAgent := strings.TrimSpace(q.Get("user_agent")); userAgent != "" && !strings.Contains(strings.ToLower(record.UserAgent), strings.ToLower(userAgent)) {
		return false
	}
	model, provider, credential := q.Get("model"), q.Get("provider"), q.Get("credential")
	modelMatch := model == "" || record.Model == model || record.RequestedModel == model
	providerMatch := provider == "" || record.Provider == provider
	credentialMatch := credential == "" || record.AuthID == credential || record.AuthIndex == credential
	parts := []string{record.ID, record.TraceID, record.SessionID, record.ParentSessionID, record.Model, record.RequestedModel, record.ResponseModel, record.ReasoningEffort, record.ServiceTier, record.OutboundServiceTier, record.ResponseServiceTier, normalizedServiceTier(record.ServiceTier), normalizedServiceTier(record.OutboundServiceTier), normalizedServiceTier(record.ResponseServiceTier), record.Provider, record.AuthID, record.AuthIndex, record.APIKey, record.ClientIP, record.UserAgent, record.Key, record.Error}
	for _, event := range record.Executions {
		modelMatch = modelMatch || event.Model == model || event.Alias == model
		providerMatch = providerMatch || event.Provider == provider
		credentialMatch = credentialMatch || event.AuthID == credential || event.AuthIndex == credential
		parts = append(parts, event.ID, event.Model, event.ResponseModel, event.ReasoningEffort, event.ServiceTier, event.OutboundServiceTier, event.ResponseServiceTier, normalizedServiceTier(event.ServiceTier), normalizedServiceTier(event.OutboundServiceTier), normalizedServiceTier(event.ResponseServiceTier), event.AuthID, event.AuthIndex)
	}
	for _, selection := range record.Selections {
		credentialMatch = credentialMatch || selection.AuthID == credential || selection.AuthIndex == credential
		parts = append(parts, selection.AuthID, selection.AuthIndex)
	}
	if !modelMatch || !providerMatch || !credentialMatch || (q.Get("outcome") != "" && q.Get("outcome") != record.Outcome) {
		return false
	}
	return strings.Contains(strings.ToLower(strings.Join(parts, "\n")), strings.ToLower(q.Get("q")))
}

func groupsSorted(groups map[string]*reportGroup) []reportGroup {
	list := make([]reportGroup, 0, len(groups))
	for _, group := range groups {
		list = append(list, *group)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Total == list[j].Total {
			return list[i].Name < list[j].Name
		}
		return list[i].Total > list[j].Total
	})
	return list
}

func addRequestGroups(record reportRequest, models, providers, credentials, keys map[string]*reportGroup) {
	seen := make(map[*reportGroup]bool)
	groupFor := func(groups map[string]*reportGroup, key string) *reportGroup {
		if key == "" {
			key = "未知"
		}
		group := groups[key]
		if group == nil {
			group = &reportGroup{Name: key}
			groups[key] = group
		}
		if !seen[group] {
			group.Requests++
			seen[group] = true
		}
		return group
	}
	for _, event := range record.Executions {
		credential := event.AuthIndex
		if credential == "" {
			credential = event.AuthID
		}
		for _, group := range []*reportGroup{groupFor(models, event.Model), groupFor(credentials, credential), groupFor(providers, event.Provider)} {
			group.reportMetrics.add(executionMetrics(event))
			group.Prices = appendPriceQuote(group.Prices, event.Price)
		}
	}
	for _, selection := range record.Selections {
		credential := selection.AuthIndex
		if credential == "" {
			credential = selection.AuthID
		}
		provider := selection.Provider
		for _, event := range record.Executions {
			if (selection.AuthID != "" && event.AuthID == selection.AuthID) || (selection.AuthIndex != "" && event.AuthIndex == selection.AuthIndex) {
				provider = event.Provider
				break
			}
		}
		for _, group := range []*reportGroup{groupFor(models, modelName(selection.Model)), groupFor(credentials, credential), groupFor(providers, provider)} {
			group.QuotaMs += selection.ActualMs
			group.QuotaPlannedMs += selection.PlannedMs
			if !selection.WaitStartedAt.IsZero() && selection.WaitFinishedAt.IsZero() && record.Outcome != "running" {
				group.QuotaUnknown++
			}
		}
	}
	model := modelName(record.Model)
	if model == "" {
		model = modelName(record.RequestedModel)
	}
	if len(record.Executions) > 0 {
		model = record.Executions[0].Model
	}
	if len(record.Admissions) > 0 || (len(record.Executions) == 0 && len(record.Selections) == 0) {
		for _, group := range []*reportGroup{groupFor(models, model), groupFor(providers, record.Provider)} {
			for _, admission := range record.Admissions {
				group.ManualMs += admission.ActualMs
				if admission.FinishedAt.IsZero() && record.Outcome != "running" {
					group.ManualUnknown++
				}
			}
		}
	}
	// Each client request belongs to one API key, independent of upstream retries.
	key := clientKeyLabel(record)
	client := keys[key]
	if client == nil {
		client = &reportGroup{Name: key}
		keys[key] = client
	}
	client.reportMetrics.add(requestMetrics(record))
	for _, execution := range record.Executions {
		client.Prices = appendPriceQuote(client.Prices, execution.Price)
	}
}

func (s *reportStore) query(q url.Values, now time.Time) (reportResponse, int, error) {
	start, end, errRange := reportRange(q, now)
	if errRange != nil {
		return reportResponse{}, http.StatusBadRequest, errRange
	}
	bucket := q.Get("bucket")
	if bucket == "" {
		bucket = "day"
	}
	if bucket != "hour" && bucket != "day" && bucket != "month" {
		return reportResponse{}, http.StatusBadRequest, fmt.Errorf("invalid bucket")
	}
	// The current interval is partial; future intervals must not look like zero usage.
	queryEnd := end.UnixMilli()
	if end.After(now) {
		end = now.Add(time.Nanosecond)
		queryEnd = now.UnixMilli() + 1
	}
	bins, errTimeline := reportTimeline(start, end, bucket)
	if errTimeline != nil {
		return reportResponse{}, http.StatusBadRequest, errTimeline
	}
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	if pageSize <= 0 {
		pageSize = 50
	}
	page, pageSize = max(1, min(page, 1000000)), min(pageSize, 200)
	result := reportResponse{Page: page, PageSize: pageSize, CollectedAt: now, Timezone: now.In(time.Local).Format("MST"), ServerDate: now.In(time.Local).Format(time.DateOnly), Bucket: bucket,
		Events: []reportRow{}, Buckets: []reportBucket{}, Models: []reportGroup{}, Providers: []reportGroup{}, Credentials: []reportGroup{}, Keys: []reportGroup{},
		ModelOptions: []string{}, ProviderOptions: []string{}, PriceSource: officialPrices.Source, PriceDate: officialPrices.FetchedAt,
		TierPriceSource: officialTierPrices.Source, TierPriceDate: officialTierPrices.FetchedAt}
	s.mu.Lock()
	result.Enabled, result.DBPath, result.CollectErr, result.LastWrite = s.enabled, s.path, s.lastErr, s.lastWrite
	db := s.db
	s.mu.Unlock()
	if !result.Enabled {
		return result, http.StatusOK, nil
	}
	if db == nil {
		return result, http.StatusServiceUnavailable, fmt.Errorf("usage database is unavailable: %s", result.CollectErr)
	}
	rows, errQuery := db.Query("SELECT payload FROM usage_events WHERE ts >= ? AND ts < ? ORDER BY ts DESC, id DESC", start.UnixMilli(), queryEnd)
	if errQuery != nil {
		return result, http.StatusInternalServerError, fmt.Errorf("query usage: %w", errQuery)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			s.mu.Lock()
			s.failLocked(fmt.Errorf("close usage rows: %w", errClose))
			s.mu.Unlock()
		}
	}()
	result.Buckets = bins
	models, providers, credentials, keys := make(map[string]*reportGroup), make(map[string]*reportGroup), make(map[string]*reportGroup), make(map[string]*reportGroup)
	modelOptions, providerOptions := make(map[string]bool), make(map[string]bool)
	for rows.Next() {
		var raw []byte
		if errScan := rows.Scan(&raw); errScan != nil {
			return result, http.StatusInternalServerError, fmt.Errorf("read usage row: %w", errScan)
		}
		var record reportRequest
		if errDecode := json.Unmarshal(raw, &record); errDecode != nil {
			result.Skipped++
			continue
		}
		s.snapshot(&record, now)
		for _, model := range []string{record.Model, record.RequestedModel} {
			if model != "" {
				modelOptions[model] = true
			}
		}
		if record.Provider != "" {
			providerOptions[record.Provider] = true
		}
		for _, execution := range record.Executions {
			if execution.Model != "" {
				modelOptions[execution.Model] = true
			}
			if execution.Provider != "" {
				providerOptions[execution.Provider] = true
			}
		}
		if !reportMatches(record, q) {
			continue
		}
		priceRequest(&record)
		metrics := requestMetrics(record)
		result.Summary.add(metrics)
		result.Total++
		if result.Total > int64((page-1)*pageSize) && len(result.Events) < pageSize {
			result.Events = append(result.Events, reportRow{ID: record.ID, TraceID: record.TraceID, StartedAt: record.StartedAt,
				CompletedAt: record.CompletedAt, Model: record.Model, RequestedModel: record.RequestedModel, Provider: record.Provider,
				ResponseModel: record.ResponseModel, ReasoningEffort: record.ReasoningEffort,
				ServiceTier: record.ServiceTier, OutboundServiceTier: record.OutboundServiceTier, ResponseServiceTier: record.ResponseServiceTier, AuthType: record.AuthType,
				APIKey: record.APIKey, ClientIP: record.ClientIP, UserAgent: record.UserAgent, Key: record.Key, SessionID: record.SessionID, AuthID: record.AuthID, AuthIndex: record.AuthIndex, Stream: record.Stream,
				Outcome: record.Outcome, StatusCode: record.StatusCode, SelectionCount: len(record.Selections), Error: record.Error, Prices: requestPriceQuotes(record), reportMetrics: metrics})
		}
		i := sort.Search(len(result.Buckets), func(i int) bool { return result.Buckets[i].StartedAt.After(record.StartedAt) }) - 1
		if i >= 0 {
			result.Buckets[i].reportMetrics.add(metrics)
		}
		addRequestGroups(record, models, providers, credentials, keys)
	}
	if errRows := rows.Err(); errRows != nil {
		return result, http.StatusInternalServerError, fmt.Errorf("iterate usage rows: %w", errRows)
	}
	result.Models, result.Providers, result.Credentials, result.Keys = groupsSorted(models), groupsSorted(providers), groupsSorted(credentials), groupsSorted(keys)
	for model := range modelOptions {
		result.ModelOptions = append(result.ModelOptions, model)
	}
	for provider := range providerOptions {
		result.ProviderOptions = append(result.ProviderOptions, provider)
	}
	sort.Strings(result.ModelOptions)
	sort.Strings(result.ProviderOptions)
	return result, http.StatusOK, nil
}

func (c *control) usageReport(q url.Values) pluginapi.ManagementResponse {
	result, status, errQuery := c.report.query(q, c.now())
	if errQuery != nil {
		return jsonResponse(status, map[string]string{"error": errQuery.Error()})
	}
	result.CredentialLabels = make(map[string]string)
	if auths, errList := c.auths(); errList == nil {
		for _, auth := range auths {
			label := auth.Label
			if label == "" {
				label = auth.Name
			}
			if label == "" {
				label = auth.ID
			}
			result.CredentialLabels[auth.ID], result.CredentialLabels[auth.AuthIndex] = label, label
		}
	}
	return jsonResponse(http.StatusOK, result)
}

func (c *control) usageRequest(q url.Values) pluginapi.ManagementResponse {
	if q.Get("id") == "" {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "id is required"})
	}
	s := c.report
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.db == nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "usage database is unavailable or disabled"})
	}
	var raw []byte
	errRead := s.db.QueryRow("SELECT payload FROM usage_events WHERE dedup = ?", q.Get("id")).Scan(&raw)
	if errRead == sql.ErrNoRows {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "request record not found"})
	}
	if errRead != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "cannot read request record"})
	}
	var record reportRequest
	if errDecode := json.Unmarshal(raw, &record); errDecode != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "cannot decode request record"})
	}
	s.snapshot(&record, c.now())
	record.RunID = ""
	priceRequest(&record)
	return jsonResponse(http.StatusOK, map[string]any{"request": record, "summary": requestMetrics(record), "price_source": officialPrices.Source, "price_date": officialPrices.FetchedAt, "tier_price_source": officialTierPrices.Source, "tier_price_date": officialTierPrices.FetchedAt})
}
