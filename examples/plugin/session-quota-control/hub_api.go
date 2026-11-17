package main

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func (c *control) serveHub(req pluginapi.ManagementRequest, path string) pluginapi.ManagementResponse {
	if req.Method == http.MethodGet {
		switch path {
		case "/hub/state":
			return c.hub.state()
		case "/hub/record":
			id, _ := strconv.ParseInt(req.Query.Get("channel_id"), 10, 64)
			return c.hub.record(req.Query.Get("id"), id)
		}
	}
	if req.Method == http.MethodPost {
		var request struct {
			ChannelIDs []int64 `json:"channel_ids"`
			JobID      string  `json:"job_id"`
		}
		if len(req.Body) > 65536 {
			return jsonResponse(400, map[string]string{"error": "Hub 请求过大"})
		}
		if len(req.Body) != 0 {
			if errDecode := json.Unmarshal(req.Body, &request); errDecode != nil {
				return jsonResponse(400, map[string]string{"error": "Hub 请求格式错误"})
			}
		}
		var errAction error
		switch path {
		case "/hub/settings":
			errAction = c.hub.settings(req.Body)
		case "/hub/cancel":
			c.hub.cancelJob()
		case "/hub/discover":
			errAction = c.hub.launch("discover", nil, "")
		case "/hub/key-channels":
			errAction = c.hub.launch("key-channels", nil, "")
		case "/hub/detect":
			errAction = c.hub.launch("detect", request.ChannelIDs, "")
		case "/hub/apply":
			errAction = c.hub.launch("apply", nil, request.JobID)
		case "/hub/recover":
			errAction = c.hub.launch("recover", nil, "")
		default:
			return jsonResponse(404, map[string]string{"error": "route not found"})
		}
		if errAction != nil {
			return jsonResponse(400, map[string]string{"error": errAction.Error()})
		}
		return c.hub.state()
	}
	return jsonResponse(404, map[string]string{"error": "route not found"})
}
