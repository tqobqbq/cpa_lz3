package pluginhost

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

func (h *Host) HasSessionAffinityGate() bool {
	if h == nil {
		return false
	}
	for _, record := range h.activeRecords() {
		if !h.isPluginFused(record.id) && record.plugin.Capabilities.SessionAffinityGate != nil {
			return true
		}
	}
	return false
}

func (h *Host) AwaitSessionAffinity(ctx context.Context, req pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil {
		return pluginapi.SessionAffinityResponse{}, nil
	}
	req.RequestID = coreexecutor.RequestLifecycleIDFromContext(ctx)
	req.TraceID = logging.GetRequestID(ctx)
	req.APIKey = coreusage.APIKeyFromContext(ctx)
	for _, record := range h.activeRecords() {
		gate := record.plugin.Capabilities.SessionAffinityGate
		if gate == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
			continue
		}
		resp := h.callAffinityGate(ctx, record, gate, req)
		if errContext := ctx.Err(); errContext != nil {
			return pluginapi.SessionAffinityResponse{}, errContext
		}
		if resp.AuthID != "" {
			return resp, nil
		}
		// Only the highest-priority gate owns admission for this session.
		break
	}
	return pluginapi.SessionAffinityResponse{}, nil
}

func (h *Host) callAffinityGate(ctx context.Context, record capabilityRecord, gate pluginapi.SessionAffinityGate, req pluginapi.SessionAffinityRequest) (resp pluginapi.SessionAffinityResponse) {
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "SessionAffinityGate.AwaitSessionAffinity", recovered)
			resp = pluginapi.SessionAffinityResponse{}
		}
	}()
	result, errAwait := gate.AwaitSessionAffinity(ctx, req)
	if errAwait != nil {
		log.WithField("plugin_id", record.id).WithError(errAwait).Warn("pluginhost: session admission failed")
		return pluginapi.SessionAffinityResponse{}
	}
	return result
}
