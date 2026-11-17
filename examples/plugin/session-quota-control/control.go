package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type control struct {
	mu            sync.Mutex
	cfg           config
	closed        bool
	sequence      uint64
	pending       map[string]*pendingSession
	history       []affinityHistoryRecord
	pools         map[string]*quotaPool
	active        map[string]*activeRequest
	models        map[string]*modelAverage
	waiting       int
	extensionBase string
	resourceBase  string
	now           func() time.Time
	wait          func(string, time.Duration) error
	quota         func(string) (pluginapi.HostAuthQuotaResponse, error)
	auths         func() ([]pluginapi.HostAuthFileEntry, error)
	apiKeys       func() ([]string, error)
	report        *reportStore
	evaluations   *evaluationManager
	hub           *hubManager
}

type pendingSession struct {
	APIKey       string          `json:"api_key"`
	ID           string          `json:"id"`
	SessionID    string          `json:"session_id"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	CreatedAt    time.Time       `json:"created_at"`
	Deadline     time.Time       `json:"deadline"`
	Candidates   []candidateView `json:"candidates"`
	Waiters      int             `json:"waiters"`
	Affinity     map[string]any  `json:"affinity"`
	authID       string
	released     bool
	accessDenied bool
}

type candidateView struct {
	ID         string            `json:"id"`
	AuthIndex  string            `json:"auth_index"`
	Provider   string            `json:"provider"`
	Priority   int               `json:"priority"`
	Status     string            `json:"status"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type affinityHistoryRecord struct {
	Session      pendingSession `json:"session"`
	Result       string         `json:"result"`
	ManualAuthID string         `json:"manual_auth_id,omitempty"`
	FinishedAt   time.Time      `json:"finished_at"`
}

func newController() *control {
	c := &control{cfg: defaultConfig(), pending: make(map[string]*pendingSession), pools: make(map[string]*quotaPool),
		active: make(map[string]*activeRequest), models: make(map[string]*modelAverage), now: time.Now, wait: waitForHost, report: newReportStore(),
		quota: func(index string) (pluginapi.HostAuthQuotaResponse, error) {
			var response pluginapi.HostAuthQuotaResponse
			errQuery := callHost(pluginabi.MethodHostAuthQuota, pluginapi.HostAuthGetRequest{AuthIndex: index}, &response)
			return response, errQuery
		},
		apiKeys: func() ([]string, error) {
			var response pluginapi.HostAPIKeysResponse
			errList := callHost(pluginabi.MethodHostAPIKeysList, struct{}{}, &response)
			return response.APIKeys, errList
		},
		auths: func() ([]pluginapi.HostAuthFileEntry, error) {
			var response struct {
				Files []pluginapi.HostAuthFileEntry `json:"files"`
			}
			errList := callHost(pluginabi.MethodHostAuthList, pluginapi.HostAuthListRequest{IncludeRuntime: true}, &response)
			if errList != nil {
				return nil, errList
			}
			var details struct {
				Files []pluginapi.HostAuthFileEntry `json:"files"`
			}
			if errDetails := callHost(pluginabi.MethodHostAuthList, pluginapi.HostAuthListRequest{}, &details); errDetails == nil {
				byID := make(map[string]pluginapi.HostAuthFileEntry, len(details.Files))
				for _, entry := range details.Files {
					byID[entry.ID] = entry
				}
				for i, entry := range response.Files {
					if detail, exists := byID[entry.ID]; exists {
						detail.Status, detail.Disabled, detail.Unavailable, detail.Priority = entry.Status, entry.Disabled, entry.Unavailable, entry.Priority
						response.Files[i] = detail
					}
				}
			}
			return response.Files, nil
		}}
	c.evaluations = newEvaluationManager(func() ([]pluginapi.HostAuthFileEntry, error) { return c.auths() })
	c.hub = newHubManager()
	return c
}

func waitForHost(callbackID string, delay time.Duration) error {
	var response pluginapi.HostContextWaitResponse
	ms := int((delay + time.Millisecond - 1) / time.Millisecond)
	if ms < 0 {
		ms = 0
	}
	if errWait := callHost(pluginabi.MethodHostContextWait, pluginapi.HostContextWaitRequest{HostCallbackID: callbackID, Milliseconds: ms}, &response); errWait != nil {
		return errWait
	}
	if response.Canceled {
		return context.Canceled
	}
	return nil
}

func (c *control) stop() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.evaluations.stop()
	c.hub.stop()
	c.report.close()
}

func (c *control) handle(method string, raw []byte) (any, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := c.configure(raw); errConfigure != nil {
			return nil, errConfigure
		}
		return registration(), nil
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		c.stop()
		return struct{}{}, nil
	case pluginabi.MethodSessionAffinityAwait:
		var req affinityCall
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, errDecode
		}
		return c.awaitAssignment(req)
	case pluginabi.MethodAuthCandidatesFilter:
		var req pluginapi.AuthCandidateFilterRequest
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, errDecode
		}
		return c.filterAccess(req), nil
	case pluginabi.MethodRequestInterceptBefore:
		var req interceptCall
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, errDecode
		}
		c.reportBegin(req)
		return pluginapi.RequestInterceptResponse{}, nil
	case pluginabi.MethodRequestInterceptAfter:
		var req interceptCall
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, errDecode
		}
		return c.pace(req), nil
	case pluginabi.MethodRequestComplete:
		var req pluginapi.RequestCompletion
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, errDecode
		}
		c.mu.Lock()
		c.finishLocked(req.RequestID, req.CompletedAt)
		c.mu.Unlock()
		c.reportComplete(req)
		return struct{}{}, nil
	case pluginabi.MethodUsageHandle:
		var req pluginapi.UsageRecord
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, errDecode
		}
		c.recordUsage(req)
		c.reportUsage(req)
		return struct{}{}, nil
	case pluginabi.MethodManagementRegister:
		var req pluginapi.ManagementRegistrationRequest
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, errDecode
		}
		c.mu.Lock()
		c.extensionBase, c.resourceBase = req.ExtensionBasePath, req.ResourceBasePath
		c.mu.Unlock()
		return map[string]any{
			"resources": []map[string]string{
				{"Path": "/dashboard", "Menu": "Session & quota control", "Description": "Session scheduling, credential quota and wait settings, usage statistics and request history."},
				{"Path": "/report", "Description": "Open the usage tab in the shared dashboard."},
			},
			"extension_routes": []map[string]string{{"Method": "GET", "Path": "/state"}, {"Method": "POST", "Path": "/assign"},
				{"Method": "GET", "Path": "/access"},
				{"Method": "GET", "Path": "/usage"}, {"Method": "GET", "Path": "/usage/request"},
				{"Method": "GET", "Path": "/evaluations"}, {"Method": "POST", "Path": "/evaluations"},
				{"Method": "GET", "Path": "/evaluations/catalog"}, {"Method": "GET", "Path": "/evaluations/record"},
				{"Method": "POST", "Path": "/evaluations/cancel"},
				{"Method": "GET", "Path": "/hub/state"}, {"Method": "GET", "Path": "/hub/record"},
				{"Method": "POST", "Path": "/hub/settings"}, {"Method": "POST", "Path": "/hub/discover"},
				{"Method": "POST", "Path": "/hub/key-channels"}, {"Method": "POST", "Path": "/hub/detect"},
				{"Method": "POST", "Path": "/hub/cancel"}, {"Method": "POST", "Path": "/hub/apply"},
				{"Method": "POST", "Path": "/hub/recover"}},
		}, nil
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, errDecode
		}
		return c.serve(req), nil
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

func (c *control) awaitAssignment(req affinityCall) (response pluginapi.SessionAffinityResponse, err error) {
	c.mu.Lock()
	if c.closed || !c.cfg.ManualAssignment || c.cfg.AssignmentWait == 0 {
		c.mu.Unlock()
		return pluginapi.SessionAffinityResponse{}, nil
	}
	now := c.now()
	identity := req.SessionID
	if identity == "" {
		c.sequence++
		identity = fmt.Sprintf("unidentified:%d", c.sequence)
	}
	digest := sha256.Sum256([]byte(req.APIKey + "\x00" + req.Provider + "\x00" + req.Model + "\x00" + identity))
	key := hex.EncodeToString(digest[:])
	pending := c.pending[key]
	if pending == nil {
		if len(c.pending)+c.waiting >= c.cfg.MaxPending {
			c.mu.Unlock()
			return pluginapi.SessionAffinityResponse{}, nil
		}
		c.sequence++
		pending = &pendingSession{ID: fmt.Sprintf("%d-%d", now.UnixNano(), c.sequence), APIKey: req.APIKey, SessionID: req.SessionID,
			Provider: req.Provider, Model: req.Model, CreatedAt: now, Deadline: now.Add(time.Duration(c.cfg.AssignmentWait) * time.Second), Affinity: req.Diagnostics}
		for _, candidate := range req.Candidates {
			pending.Candidates = append(pending.Candidates, candidateView{ID: candidate.ID, AuthIndex: candidate.AuthIndex, Provider: candidate.Provider, Priority: candidate.Priority, Status: candidate.Status, Attributes: candidate.Attributes})
		}
		c.pending[key] = pending
	}
	pending.Waiters++
	c.mu.Unlock()
	admissionID := c.reportAdmissionStart(req)
	defer func() {
		c.mu.Lock()
		pending.Waiters--
		result := "timeout"
		switch {
		case err != nil:
			result = "canceled"
		case pending.accessDenied:
			result = "access_denied"
		case response.AuthID != "":
			result = "manual_assignment"
		case c.closed || !c.cfg.ManualAssignment:
			result = "disabled"
		case pending.released:
			result = "automatic_release"
		}
		if pending.Waiters == 0 {
			delete(c.pending, key)
			c.history = append(c.history, affinityHistoryRecord{Session: *pending, Result: result, ManualAuthID: response.AuthID, FinishedAt: c.now()})
			if len(c.history) > 50 {
				c.history = append([]affinityHistoryRecord(nil), c.history[len(c.history)-50:]...)
			}
		}
		c.mu.Unlock()
		c.reportAdmissionEnd(req.RequestID, admissionID, result, response.AuthID)
	}()
	for {
		c.mu.Lock()
		now = c.now()
		deadline := pending.CreatedAt.Add(time.Duration(c.cfg.AssignmentWait) * time.Second)
		pending.Deadline = deadline
		if len(pending.Candidates) > 0 {
			allowed := make([]candidateView, 0, len(pending.Candidates))
			for _, candidate := range pending.Candidates {
				if c.cfg.AccessControl.denial(pending.APIKey, candidate.AuthIndex) == "" {
					allowed = append(allowed, candidate)
				}
			}
			pending.Candidates = allowed
			pending.accessDenied = len(allowed) == 0
		}
		done := c.closed || !c.cfg.ManualAssignment || pending.released || pending.accessDenied || !now.Before(deadline)
		authID := pending.authID
		c.mu.Unlock()
		if done {
			return pluginapi.SessionAffinityResponse{AuthID: authID}, nil
		}
		if errWait := c.wait(req.CallbackID, min(250*time.Millisecond, deadline.Sub(now))); errWait != nil {
			return pluginapi.SessionAffinityResponse{}, errWait
		}
	}
}

func terminated(status int, message string) pluginapi.RequestInterceptResponse {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "quota_pacing", "message": message}})
	return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: status,
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}}, ResponseBody: body}
}
