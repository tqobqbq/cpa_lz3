package pluginhost

import (
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func (h *Host) HasAuthCandidateFilters() bool {
	if h != nil {
		for _, record := range h.Snapshot().records {
			if record.plugin.Capabilities.AuthCandidateFilter != nil {
				return true
			}
		}
	}
	return false
}

func (h *Host) FilterAuthCandidates(ctx context.Context, req pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
	result := pluginapi.AuthCandidateFilterResponse{Denied: make(map[string]string)}
	if h == nil {
		return result, nil
	}
	// Keep stale filter records visible during a snapshot replacement; their
	// availability check must reject requests instead of skipping enforcement.
	for _, record := range h.Snapshot().records {
		if record.plugin.Capabilities.AuthCandidateFilter == nil {
			continue
		}
		// A plugin's own management-initiated model checks have no client key.
		// Nested requests with an authenticated client always retain access checks.
		if req.APIKey == "" && hostCallbackPluginIDFromContext(ctx) == record.id {
			continue
		}
		response, errFilter := h.callAuthCandidateFilter(ctx, record, req)
		if errFilter != nil {
			return pluginapi.AuthCandidateFilterResponse{}, errFilter
		}
		for id, reason := range response.Denied {
			if !schedulerCandidateExists(req.Candidates, id) {
				return pluginapi.AuthCandidateFilterResponse{}, fmt.Errorf("plugin %s returned an unknown credential", record.id)
			}
			if strings.TrimSpace(reason) == "" {
				reason = "credential access denied"
			}
			result.Denied[id] = reason
		}
	}
	return result, nil
}

func (h *Host) callAuthCandidateFilter(ctx context.Context, record capabilityRecord, req pluginapi.AuthCandidateFilterRequest) (resp pluginapi.AuthCandidateFilterResponse, err error) {
	if h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return resp, fmt.Errorf("credential access filter %s is unavailable", record.id)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "AuthCandidateFilter.FilterAuthCandidates", recovered)
			resp = pluginapi.AuthCandidateFilterResponse{}
			err = fmt.Errorf("credential access filter %s failed", record.id)
		}
	}()
	req.Plugin = record.meta
	return record.plugin.Capabilities.AuthCandidateFilter.FilterAuthCandidates(ctx, req)
}
