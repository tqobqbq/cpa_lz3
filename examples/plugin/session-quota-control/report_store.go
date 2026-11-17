package main

// SQLite event storage is adapted from CPA Usage Report v0.3.1 (MIT).
// See THIRD_PARTY_NOTICES.md for the original license.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

type reportStore struct {
	mu        sync.Mutex
	db        *sql.DB
	enabled   bool
	path      string
	lastErr   string
	lastWrite time.Time
	runID     string
	historyAt time.Time
	history   map[string]*credentialHistory
}

type reportRequest struct {
	ID                  string            `json:"id"`
	TraceID             string            `json:"trace_id"`
	RunID               string            `json:"run_id"`
	StartedAt           time.Time         `json:"started_at"`
	CompletedAt         time.Time         `json:"completed_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
	Model               string            `json:"model"`
	RequestedModel      string            `json:"requested_model"`
	ResponseModel       string            `json:"response_model,omitempty"`
	ReasoningEffort     string            `json:"reasoning_effort,omitempty"`
	ServiceTier         string            `json:"service_tier,omitempty"`
	OutboundServiceTier string            `json:"outbound_service_tier,omitempty"`
	ResponseServiceTier string            `json:"response_service_tier,omitempty"`
	Provider            string            `json:"provider"`
	SessionID           string            `json:"session_id"`
	ParentSessionID     string            `json:"parent_session_id"`
	AuthID              string            `json:"auth_id"`
	AuthIndex           string            `json:"auth_index"`
	AuthType            string            `json:"auth_type,omitempty"`
	Key                 string            `json:"key,omitempty"`
	APIKey              string            `json:"api_key,omitempty"`
	ClientIP            string            `json:"client_ip,omitempty"`
	UserAgent           string            `json:"user_agent,omitempty"`
	Stream              bool              `json:"stream"`
	Outcome             string            `json:"outcome"`
	StatusCode          int               `json:"status_code"`
	Error               string            `json:"error,omitempty"`
	Admissions          []reportAdmission `json:"admissions,omitempty"`
	Selections          []reportSelection `json:"selections,omitempty"`
	Executions          []reportExecution `json:"executions,omitempty"`
}

type reportAdmission struct {
	ID             string    `json:"id"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	ActualMs       float64   `json:"actual_ms"`
	Result         string    `json:"result"`
	AuthID         string    `json:"auth_id,omitempty"`
	PreviousAuthID string    `json:"previous_auth_id,omitempty"`
}

type reportSelection struct {
	ID                   string        `json:"id"`
	SelectedAt           time.Time     `json:"selected_at"`
	Provider             string        `json:"provider"`
	Model                string        `json:"model"`
	AuthID               string        `json:"auth_id"`
	AuthIndex            string        `json:"auth_index"`
	PlannedMs            float64       `json:"planned_ms"`
	ActualMs             float64       `json:"actual_ms"`
	WaitStartedAt        time.Time     `json:"wait_started_at"`
	WaitFinishedAt       time.Time     `json:"wait_finished_at"`
	Reason               string        `json:"reason"`
	EstimatedCostPercent float64       `json:"estimated_cost_percent"`
	EstimatedConcurrency float64       `json:"estimated_concurrency"`
	Windows              []quotaWindow `json:"quota_windows,omitempty"`
	QuotaObservedAt      time.Time     `json:"quota_observed_at"`
}

type reportExecution struct {
	ID                  string                `json:"id"`
	Timestamp           time.Time             `json:"timestamp"`
	Provider            string                `json:"provider"`
	Model               string                `json:"model"`
	Alias               string                `json:"alias"`
	ResponseModel       string                `json:"response_model,omitempty"`
	ReasoningEffort     string                `json:"reasoning_effort,omitempty"`
	ServiceTier         string                `json:"service_tier,omitempty"`
	OutboundServiceTier string                `json:"outbound_service_tier,omitempty"`
	ResponseServiceTier string                `json:"response_service_tier,omitempty"`
	AuthID              string                `json:"auth_id"`
	AuthIndex           string                `json:"auth_index"`
	AuthType            string                `json:"auth_type,omitempty"`
	Key                 string                `json:"key,omitempty"`
	Failed              bool                  `json:"failed"`
	StatusCode          int                   `json:"status_code"`
	LatencyMs           float64               `json:"latency_ms"`
	TTFTMs              float64               `json:"ttft_ms"`
	Tokens              pluginapi.UsageDetail `json:"tokens"`
	Price               *priceQuote           `json:"price,omitempty"`
}

func newReportStore() *reportStore { return &reportStore{runID: uuid.NewString()} }

func clientKeyLabel(record reportRequest) string {
	if record.APIKey != "" {
		return record.APIKey
	}
	if record.Key != "" && record.Key != "未知 Key" {
		return "历史匿名标识（无法还原）：" + record.Key
	}
	return "未采集 API Key"
}

func setRequestSource(record *reportRequest, apiKey, clientIP, userAgent string) {
	if record.APIKey == "" && apiKey != "" {
		record.APIKey = apiKey
		record.Key = ""
	}
	if record.ClientIP == "" && clientIP != "" {
		record.ClientIP = clientIP
	}
	if record.UserAgent == "" && userAgent != "" {
		record.UserAgent = userAgent
	}
}

func reportPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		homeDir, errHome := os.UserHomeDir()
		if errHome != nil {
			return "", fmt.Errorf("resolve usage database home: %w", errHome)
		}
		if path == "~" {
			path = homeDir
		} else {
			path = filepath.Join(homeDir, strings.TrimPrefix(path, "~/"))
		}
	}
	return filepath.Abs(path)
}

func (s *reportStore) failLocked(err error) {
	message := err.Error()
	if s.lastErr != message {
		log.WithError(err).Warn("session-quota-control: usage storage failed")
	}
	s.lastErr = message
}

func (s *reportStore) closeLocked() {
	if s.db != nil {
		if errClose := s.db.Close(); errClose != nil {
			log.WithError(errClose).Warn("session-quota-control: close usage database")
		}
		s.db = nil
	}
}

func (s *reportStore) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = false
	s.closeLocked()
}

func (s *reportStore) configure(enabled bool, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resolved, errPath := reportPath(path)
	if enabled && errPath == nil && s.enabled && s.db != nil && s.path == resolved {
		return
	}
	s.closeLocked()
	s.enabled, s.path, s.lastErr = enabled, resolved, ""
	s.history, s.historyAt = nil, time.Time{}
	if !enabled {
		return
	}
	if errPath != nil {
		s.failLocked(errPath)
		return
	}
	if errMkdir := os.MkdirAll(filepath.Dir(resolved), 0o700); errMkdir != nil {
		s.failLocked(fmt.Errorf("create usage database directory: %w", errMkdir))
		return
	}
	// Reserve a private file before SQLite creates its database and WAL files.
	file, errCreate := os.OpenFile(resolved, os.O_CREATE|os.O_RDWR, 0o600)
	if errCreate != nil {
		s.failLocked(fmt.Errorf("create usage database: %w", errCreate))
		return
	}
	if errClose := file.Close(); errClose != nil {
		s.failLocked(fmt.Errorf("close usage database file: %w", errClose))
		return
	}
	dsnURL := &url.URL{Scheme: "file", Path: filepath.ToSlash(resolved)}
	dsnQuery := url.Values{"_pragma": {"busy_timeout(5000)", "synchronous(NORMAL)"}}
	dsnURL.RawQuery = dsnQuery.Encode()
	dsn := dsnURL.String()
	conn, errOpen := sql.Open("sqlite", dsn)
	if errOpen != nil {
		s.failLocked(fmt.Errorf("open usage database: %w", errOpen))
		return
	}
	s.db = conn
	// WAL allows the report reader to run without holding up request writes.
	conn.SetMaxOpenConns(2)
	for _, statement := range []string{
		"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL",
		"CREATE TABLE IF NOT EXISTS usage_events (id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, dedup TEXT NOT NULL UNIQUE, payload TEXT NOT NULL)",
		"CREATE INDEX IF NOT EXISTS usage_events_ts ON usage_events(ts)",
	} {
		if _, errExec := conn.Exec(statement); errExec != nil {
			s.failLocked(fmt.Errorf("initialize usage database: %w", errExec))
			s.closeLocked()
			return
		}
	}
}

// change serializes read-modify-write updates so asynchronous usage and lifecycle
// callbacks can arrive in either order without overwriting each other's fields.
func (s *reportStore) change(id string, now time.Time, update func(*reportRequest)) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.db == nil {
		return
	}
	record := reportRequest{ID: id, RunID: s.runID, StartedAt: now, Outcome: "running"}
	var raw []byte
	errQuery := s.db.QueryRow("SELECT payload FROM usage_events WHERE dedup = ?", id).Scan(&raw)
	if errQuery != nil && errQuery != sql.ErrNoRows {
		s.failLocked(fmt.Errorf("read usage event: %w", errQuery))
		return
	}
	if errQuery == nil {
		if errDecode := json.Unmarshal(raw, &record); errDecode != nil {
			s.failLocked(fmt.Errorf("decode usage event: %w", errDecode))
			return
		}
	}
	update(&record)
	record.UpdatedAt = now
	encoded, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		s.failLocked(fmt.Errorf("encode usage event: %w", errMarshal))
		return
	}
	if _, errWrite := s.db.Exec("INSERT INTO usage_events(ts,dedup,payload) VALUES(?,?,?) ON CONFLICT(dedup) DO UPDATE SET ts=excluded.ts,payload=excluded.payload", record.StartedAt.UnixMilli(), id, string(encoded)); errWrite != nil {
		s.failLocked(fmt.Errorf("write usage event: %w", errWrite))
		return
	}
	s.lastWrite, s.lastErr = now, ""
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return value
}

func (c *control) reportBegin(req interceptCall) {
	c.report.change(req.RequestID, c.now(), func(record *reportRequest) {
		setRequestSource(record, req.APIKey, req.ClientIP, req.UserAgent)
		record.TraceID, record.Model, record.RequestedModel, record.Stream = req.TraceID, req.Model, req.RequestedModel, req.Stream
		if info, explicit := coresession.ExtractSessionInfo(req.Headers, req.Body, req.Metadata); explicit {
			record.SessionID, record.ParentSessionID = info.SessionID, info.ParentSessionID
		}
	})
}

func (c *control) reportComplete(req pluginapi.RequestCompletion) {
	c.report.change(req.RequestID, c.now(), func(record *reportRequest) {
		if !req.StartedAt.IsZero() {
			record.StartedAt = req.StartedAt
		}
		record.CompletedAt, record.Outcome, record.StatusCode = req.CompletedAt, string(req.Outcome), req.StatusCode
		setRequestSource(record, req.APIKey, req.ClientIP, req.UserAgent)
		errorRunes := []rune(req.Error)
		record.Error = string(errorRunes[:min(2048, len(errorRunes))])
		if authID := metadataString(req.Metadata, coreexecutor.SelectedAuthMetadataKey); authID != "" {
			record.AuthID = authID
			record.AuthIndex = metadataString(req.Metadata, coreexecutor.SelectedAuthIndexMetadataKey)
		}
		if record.TraceID == "" {
			record.TraceID = req.TraceID
		}
		if record.Model == "" {
			record.Model = req.Model
		}
		if record.RequestedModel == "" {
			record.RequestedModel = req.RequestedModel
		}
	})
}

func (c *control) reportAdmissionStart(req affinityCall) string {
	id, now := uuid.NewString(), c.now()
	c.report.change(req.RequestID, now, func(record *reportRequest) {
		record.TraceID, record.SessionID, record.Provider = req.TraceID, req.SessionID, req.Provider
		record.Admissions = append(record.Admissions, reportAdmission{ID: id, StartedAt: now,
			PreviousAuthID: metadataString(req.Diagnostics, "previous_auth_id")})
	})
	return id
}

func (c *control) reportAdmissionEnd(requestID, admissionID, result, authID string) {
	now := c.now()
	c.report.change(requestID, now, func(record *reportRequest) {
		for i := range record.Admissions {
			admission := &record.Admissions[i]
			if admission.ID == admissionID && admission.FinishedAt.IsZero() {
				admission.FinishedAt, admission.Result, admission.AuthID = now, result, authID
				admission.ActualMs = max(0, float64(now.Sub(admission.StartedAt))/float64(time.Millisecond))
			}
		}
	})
}

func (c *control) reportSelection(req interceptCall) string {
	id, now := uuid.NewString(), c.now()
	c.report.change(req.RequestID, now, func(record *reportRequest) {
		record.Provider, record.Model = req.ToFormat, req.Model
		record.AuthID = metadataString(req.Metadata, coreexecutor.SelectedAuthMetadataKey)
		record.AuthIndex = metadataString(req.Metadata, coreexecutor.SelectedAuthIndexMetadataKey)
		if sessionID := metadataString(req.Metadata, coreexecutor.CanonicalSessionIDMetadataKey); sessionID != "" {
			record.SessionID = sessionID
		}
		if parentID := metadataString(req.Metadata, coreexecutor.ParentSessionIDMetadataKey); parentID != "" {
			record.ParentSessionID = parentID
		}
		record.Selections = append(record.Selections, reportSelection{ID: id, SelectedAt: now,
			Provider: req.ToFormat, Model: req.Model, AuthID: record.AuthID, AuthIndex: record.AuthIndex, Reason: "no quota delay"})
	})
	return id
}

func (c *control) reportQuotaPlan(requestID, selectionID string, delay time.Duration, reason string, cost, concurrency float64, windows []quotaWindow, observedAt time.Time) {
	now := c.now()
	c.report.change(requestID, now, func(record *reportRequest) {
		for i := range record.Selections {
			selection := &record.Selections[i]
			if selection.ID == selectionID {
				selection.PlannedMs, selection.Reason = float64(delay)/float64(time.Millisecond), reason
				selection.EstimatedCostPercent, selection.EstimatedConcurrency, selection.Windows = cost*100, concurrency, windows
				selection.QuotaObservedAt = observedAt
				if delay > 0 {
					selection.WaitStartedAt = now
				}
			}
		}
	})
}

func (c *control) reportQuotaEnd(requestID, selectionID string, startedAt, finishedAt time.Time) {
	c.report.change(requestID, finishedAt, func(record *reportRequest) {
		for i := range record.Selections {
			selection := &record.Selections[i]
			if selection.ID == selectionID {
				selection.WaitStartedAt, selection.WaitFinishedAt = startedAt, finishedAt
				selection.ActualMs = max(0, float64(finishedAt.Sub(startedAt))/float64(time.Millisecond))
			}
		}
	})
}

func (c *control) reportUsage(record pluginapi.UsageRecord) {
	event := reportExecution{ID: record.RequestID, Timestamp: record.RequestedAt, Provider: record.Provider,
		Model: record.Model, Alias: record.Alias, AuthID: record.AuthID, AuthIndex: record.AuthIndex, AuthType: record.AuthType,
		ResponseModel: record.ResponseModel, ReasoningEffort: record.ReasoningEffort,
		ServiceTier: record.ServiceTier, OutboundServiceTier: record.OutboundServiceTier, ResponseServiceTier: record.ResponseServiceTier,
		Failed: record.Failed, StatusCode: record.Failure.StatusCode,
		LatencyMs: max(0, float64(record.Latency)/float64(time.Millisecond)),
		TTFTMs:    max(0, float64(record.TTFT)/float64(time.Millisecond)), Tokens: record.Detail}
	if event.ID == "" {
		encoded, _ := json.Marshal(event)
		digest := sha256.Sum256(encoded)
		event.ID = hex.EncodeToString(digest[:])
	}
	id := record.LifecycleRequestID
	if id == "" {
		id = "execution:" + event.ID
	}
	c.report.change(id, c.now(), func(request *reportRequest) {
		for _, previous := range request.Executions {
			if previous.ID == event.ID {
				return
			}
		}
		request.Executions = append(request.Executions, event)
		setRequestSource(request, record.APIKey, record.ClientIP, record.UserAgent)
		if request.TraceID == "" {
			request.TraceID = record.TraceID
		}
		if request.Provider == "" {
			request.Provider, request.Model = record.Provider, record.Model
			request.AuthID, request.AuthIndex = record.AuthID, record.AuthIndex
		}
		if request.SessionID == "" {
			request.SessionID, request.ParentSessionID = record.SessionID, record.ParentSessionID
		}
		if record.LifecycleRequestID == "" {
			request.StartedAt, request.CompletedAt = record.RequestedAt, record.RequestedAt.Add(record.Latency)
			if request.StartedAt.IsZero() {
				request.StartedAt, request.CompletedAt = c.now(), c.now()
			}
			request.Outcome = "succeeded"
			if record.Failed {
				request.Outcome = "failed"
			}
			request.StatusCode = record.Failure.StatusCode
		}
	})
}
