package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/examples/plugin/session-quota-control/evaluation"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func (c *control) serveEvaluations(req pluginapi.ManagementRequest, path string) pluginapi.ManagementResponse {
	fail := func(status int, message string) pluginapi.ManagementResponse {
		return jsonResponse(status, map[string]string{"error": message})
	}
	switch {
	case req.Method == http.MethodGet && path == "/evaluations/catalog":
		return jsonResponse(http.StatusOK, map[string]any{"defaults": defaultEvaluationPlan(), "models": evaluation.ModelNames()})
	case req.Method == http.MethodGet && path == "/evaluations":
		credentialID := req.Query.Get("credential_id")
		if credentialID == "" {
			return fail(http.StatusBadRequest, "请选择 Credential")
		}
		jobs, storageError, errList := c.evaluations.list(credentialID)
		if errList != nil {
			return fail(http.StatusServiceUnavailable, errList.Error())
		}
		return jsonResponse(http.StatusOK, map[string]any{"records": jobs, "storage_error": storageError, "now": c.evaluations.now()})
	case req.Method == http.MethodGet && path == "/evaluations/record":
		job, errRead := c.evaluations.record(req.Query.Get("credential_id"), req.Query.Get("id"))
		if errors.Is(errRead, sql.ErrNoRows) {
			return fail(http.StatusNotFound, "找不到检测记录")
		}
		if errRead != nil {
			return fail(http.StatusServiceUnavailable, errRead.Error())
		}
		return jsonResponse(http.StatusOK, job)
	case req.Method == http.MethodPost && path == "/evaluations":
		plan := defaultEvaluationPlan()
		if errDecode := json.Unmarshal(req.Body, &plan); errDecode != nil {
			return fail(http.StatusBadRequest, "检测参数格式错误")
		}
		if _, errValidate := plan.options(); errValidate != nil {
			return fail(http.StatusBadRequest, errValidate.Error())
		}
		auths, errAuths := c.auths()
		if errAuths != nil {
			return fail(http.StatusServiceUnavailable, "无法读取 Credential")
		}
		for _, auth := range auths {
			if auth.ID != plan.CredentialID {
				continue
			}
			if auth.Disabled || auth.Provider == "" {
				return fail(http.StatusConflict, "Credential 已禁用或缺少 Provider")
			}
			jobs, errAdd := c.evaluations.add(plan, auth.Provider)
			if errAdd != nil {
				return fail(http.StatusServiceUnavailable, errAdd.Error())
			}
			return jsonResponse(http.StatusCreated, map[string]any{"records": jobs, "count": len(jobs)})
		}
		return fail(http.StatusNotFound, "Credential 已移除")
	case req.Method == http.MethodPost && path == "/evaluations/cancel":
		var request struct {
			CredentialID string `json:"credential_id"`
			ID           string `json:"id"`
		}
		if errDecode := json.Unmarshal(req.Body, &request); errDecode != nil || request.ID == "" || request.CredentialID == "" {
			return fail(http.StatusBadRequest, "请选择要取消的检测")
		}
		if errCancel := c.evaluations.cancelJob(request.CredentialID, request.ID); errCancel != nil {
			return fail(http.StatusConflict, errCancel.Error())
		}
		return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
	default:
		return fail(http.StatusNotFound, "route not found")
	}
}
