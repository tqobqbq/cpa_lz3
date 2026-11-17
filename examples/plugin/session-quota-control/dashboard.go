package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

//go:embed dashboard.html
var dashboardHTML string

//go:embed dashboard.css
var dashboardCSS string

//go:embed credentials.html
var credentialsHTML string

//go:embed credentials.js
var credentialsJS string

//go:embed access.html
var accessHTML string

//go:embed access.js
var accessJS string

//go:embed evaluations.html
var evaluationsHTML string

//go:embed evaluations.js
var evaluationsJS string

//go:embed hub.html
var hubHTML string

//go:embed hub.js
var hubJS string

//go:embed theme.js
var themeJS string

//go:embed report.html
var reportHTML string

//go:embed scheduling.html
var schedulingHTML string

//go:embed scheduling.js
var schedulingJS string

//go:embed report.js
var reportJS string

//go:embed app.js
var appJS string

//go:embed management_auth.js
var managementAuthJS string

func jsonResponse(status int, body any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		status, raw = http.StatusInternalServerError, []byte(`{"error":"encode response"}`)
	}
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{
		"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"},
		"X-Content-Type-Options": {"nosniff"},
	}, Body: raw}
}

func (c *control) serve(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	c.mu.Lock()
	extensionBase, resourceBase := c.extensionBase, c.resourceBase
	c.mu.Unlock()
	switch {
	case req.Method == http.MethodGet && (req.Path == resourceBase+"/dashboard" || req.Path == resourceBase+"/report"):
		baseJSON, _ := json.Marshal(extensionBase)
		pluginID := strings.TrimSuffix(strings.TrimPrefix(extensionBase, "/v8/management/plugins/"), "/extension")
		configJSON, _ := json.Marshal("/v8/management/config/plugins/configs/" + pluginID)
		defaultsJSON, _ := json.Marshal(defaultConfig())
		initialView := "scheduling"
		if req.Path == resourceBase+"/report" {
			initialView = "usage"
		}
		initialViewJSON, _ := json.Marshal(initialView)
		page := strings.NewReplacer(
			"__DASHBOARD_STYLE__", dashboardCSS,
			"__THEME_SCRIPT__", themeJS,
			"__SCHEDULING_CONTENT__", schedulingHTML,
			"__CREDENTIALS_CONTENT__", credentialsHTML,
			"__ACCESS_CONTENT__", accessHTML,
			"__EVALUATIONS_CONTENT__", evaluationsHTML,
			"__HUB_CONTENT__", hubHTML,
			"__USAGE_CONTENT__", reportHTML,
			"__SCHEDULING_SCRIPT__", schedulingJS,
			"__CREDENTIALS_SCRIPT__", credentialsJS,
			"__ACCESS_SCRIPT__", accessJS,
			"__EVALUATIONS_SCRIPT__", evaluationsJS,
			"__HUB_SCRIPT__", hubJS,
			"__USAGE_SCRIPT__", reportJS,
			"__APP_SCRIPT__", appJS,
			"__MANAGEMENT_AUTH__", managementAuthJS,
		).Replace(dashboardHTML)
		page = strings.NewReplacer(
			"__EXTENSION_BASE__", string(baseJSON),
			"__CONFIG_PATH__", string(configJSON),
			"__DEFAULT_CONFIG__", string(defaultsJSON),
			"__INITIAL_VIEW__", string(initialViewJSON),
		).Replace(page)
		return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{
			"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"},
			"X-Content-Type-Options": {"nosniff"}, "Referrer-Policy": {"no-referrer"},
			"Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data: blob:; frame-src 'self'; base-uri 'none'; form-action 'none'"},
		}, Body: []byte(page)}
	case req.Method == http.MethodGet && req.Path == extensionBase+"/state":
		return c.state()
	case req.Method == http.MethodGet && req.Path == extensionBase+"/access":
		return c.accessState()
	case req.Method == http.MethodPost && req.Path == extensionBase+"/assign":
		return c.assign(req.Body)
	case req.Method == http.MethodGet && req.Path == extensionBase+"/usage":
		return c.usageReport(req.Query)
	case req.Method == http.MethodGet && req.Path == extensionBase+"/usage/request":
		return c.usageRequest(req.Query)
	case strings.HasPrefix(req.Path, extensionBase+"/evaluations"):
		return c.serveEvaluations(req, strings.TrimPrefix(req.Path, extensionBase))
	case strings.HasPrefix(req.Path, extensionBase+"/hub/"):
		return c.serveHub(req, strings.TrimPrefix(req.Path, extensionBase))
	default:
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "route not found"})
	}
}

func (c *control) assign(body []byte) pluginapi.ManagementResponse {
	var req struct {
		ID      string `json:"id"`
		AuthID  string `json:"auth_id"`
		Release bool   `json:"release"`
	}
	if errDecode := json.Unmarshal(body, &req); errDecode != nil || req.ID == "" || (!req.Release && req.AuthID == "") {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "id and auth_id (or release) are required"})
	}
	if !req.Release {
		auths, errList := c.auths()
		if errList != nil {
			return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "cannot read credentials"})
		}
		eligible := false
		for _, auth := range auths {
			if auth.ID == req.AuthID && !auth.Disabled {
				eligible = true
				break
			}
		}
		if !eligible {
			return jsonResponse(http.StatusConflict, map[string]string{"error": "credential is disabled or removed"})
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, pending := range c.pending {
		if pending.ID != req.ID {
			continue
		}
		if c.closed || !c.cfg.ManualAssignment || pending.released || !c.now().Before(pending.CreatedAt.Add(time.Duration(c.cfg.AssignmentWait)*time.Second)) {
			return jsonResponse(http.StatusConflict, map[string]string{"error": "assignment has expired or was already released"})
		}
		if !req.Release {
			eligible := false
			for _, candidate := range pending.Candidates {
				if candidate.ID == req.AuthID {
					if reason := c.cfg.AccessControl.denial(pending.APIKey, candidate.AuthIndex); reason != "" {
						return jsonResponse(http.StatusForbidden, map[string]string{"error": "credential_access_denied", "message": reason})
					}
					eligible = true
					break
				}
			}
			if !eligible {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": "credential is not a candidate for this session"})
			}
		}
		pending.authID, pending.released = req.AuthID, true
		if req.Release {
			pending.authID = ""
		}
		return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
	}
	return jsonResponse(http.StatusNotFound, map[string]string{"error": "pending session no longer exists"})
}

type poolView struct {
	AuthID                string                           `json:"auth_id"`
	AuthIndex             string                           `json:"auth_index"`
	Provider              string                           `json:"provider"`
	Windows               []quotaWindow                    `json:"windows"`
	ObservedAt            time.Time                        `json:"observed_at"`
	QuotaObservation      *pluginapi.HostAuthQuotaResponse `json:"quota_observation,omitempty"`
	QuotaObservationError string                           `json:"quota_observation_error,omitempty"`
	InFlight              int                              `json:"in_flight"`
	Sleeping              int                              `json:"sleeping"`
	Model                 string                           `json:"model"`
	EstimatedConcurrency  float64                          `json:"estimated_concurrency"`
	WaitSeconds           float64                          `json:"wait_seconds"`
	EstimatedCostPercent  float64                          `json:"estimated_cost_percent"`
	NextAdmission         time.Time                        `json:"next_admission"`
	Reason                string                           `json:"reason"`
}

type credentialView struct {
	pluginapi.HostAuthFileEntry
	QuotaObservation      *pluginapi.HostAuthQuotaResponse `json:"quota_observation,omitempty"`
	QuotaObservationError string                           `json:"quota_observation_error,omitempty"`
	History               *credentialHistory               `json:"history,omitempty"`
}

func (c *control) state() pluginapi.ManagementResponse {
	auths, errList := c.auths()
	if errList != nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "cannot read credentials"})
	}
	c.mu.Lock()
	now := c.now()
	pending := make([]pendingSession, 0, len(c.pending))
	for _, session := range c.pending {
		if session.released {
			continue
		}
		copySession := *session
		copySession.Deadline = session.CreatedAt.Add(time.Duration(c.cfg.AssignmentWait) * time.Second)
		pending = append(pending, copySession)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].CreatedAt.Before(pending[j].CreatedAt) })
	pools := make([]poolView, 0, len(c.pools))
	for index, pool := range c.pools {
		view := poolView{AuthID: pool.authID, AuthIndex: index, Provider: pool.provider, Windows: pool.windows,
			ObservedAt: pool.observedAt, EstimatedCostPercent: pool.lastCost * 100,
			Model: pool.lastModel, EstimatedConcurrency: pool.lastConcurrency, WaitSeconds: pool.lastDelay.Seconds(), Reason: pool.reason}
		for _, active := range c.active {
			if active.pool == pool {
				if active.sleeping {
					view.Sleeping++
					if view.NextAdmission.IsZero() || active.readyAt.Before(view.NextAdmission) {
						view.NextAdmission = active.readyAt
					}
				} else {
					view.InFlight++
				}
			}
		}
		if view.InFlight == 0 && view.Sleeping == 0 && now.Sub(pool.lastSeen) > 10*time.Hour {
			delete(c.pools, index)
			continue
		}
		pools = append(pools, view)
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].AuthID < pools[j].AuthID })
	models := make([]map[string]any, 0, len(c.models))
	for key, average := range c.models {
		provider, model, _ := strings.Cut(key, "\x00")
		models = append(models, map[string]any{"provider": provider, "model": model,
			"cost_percent": average.mean(c.cfg.ModelSamples) * 100, "samples": min(len(average.samples), c.cfg.ModelSamples)})
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i]["provider"].(string)+models[i]["model"].(string) < models[j]["provider"].(string)+models[j]["model"].(string)
	})
	cfg, waiting, inFlight := c.cfg, c.waiting, len(c.active)-c.waiting
	history := append([]affinityHistoryRecord(nil), c.history...)
	c.mu.Unlock()
	// Display observations are read from host memory, independently of the immutable
	// quota snapshot and delay recorded for each credential selection.
	historyByAuth, historyAvailable, historyError := c.report.credentialHistory(now)
	credentials := make([]credentialView, 0, len(auths))
	byID := make(map[string]int, len(auths))
	for _, auth := range auths {
		// Only display metadata is returned; backing paths and upstream URLs are unnecessary.
		auth.Path, auth.BaseURL = "", ""
		view := credentialView{HostAuthFileEntry: auth}
		if historyAvailable {
			history := historyByAuth[auth.AuthIndex]
			if history == nil {
				history = historyByAuth[auth.ID]
			}
			if history == nil {
				history = &credentialHistory{}
			}
			view.History = history
		}
		if (auth.Provider == "codex" || auth.Provider == "claude") && auth.AuthIndex != "" {
			snapshot, errQuota := c.quota(auth.AuthIndex)
			if errQuota != nil {
				view.QuotaObservationError = "cannot read credential quota observation"
			} else {
				view.QuotaObservation = &snapshot
			}
		}
		byID[auth.ID] = len(credentials)
		credentials = append(credentials, view)
	}
	for i := range pools {
		if index, exists := byID[pools[i].AuthID]; exists {
			pools[i].QuotaObservation = credentials[index].QuotaObservation
			pools[i].QuotaObservationError = credentials[index].QuotaObservationError
		}
	}
	return jsonResponse(http.StatusOK, map[string]any{"now": now, "config": cfg, "pending": pending,
		"affinity_history": history, "credentials": credentials, "pools": pools, "models": models, "waiting": waiting, "in_flight": inFlight,
		"credential_history_error": historyError})
}
