package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func (h *Host) callHostAPIKeysList() ([]byte, error) {
	h.mu.Lock()
	keys := make([]string, 0)
	if h.runtimeConfig != nil {
		keys = append(keys, h.runtimeConfig.APIKeys...)
	}
	h.mu.Unlock()
	return marshalRPCResult(pluginapi.HostAPIKeysResponse{APIKeys: keys})
}

func (h *Host) callHostContextWait(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostContextWaitRequest
	if errDecode := json.Unmarshal(request, &req); errDecode != nil {
		return nil, fmt.Errorf("decode context wait: %w", errDecode)
	}
	if req.HostCallbackID == "" || req.Milliseconds < 0 || req.Milliseconds > int((24*time.Hour)/time.Millisecond) {
		return nil, fmt.Errorf("context wait requires a callback ID and 0..86400000 milliseconds")
	}
	operation, errAcquire := h.acquireHostHTTPOperation(ctx, req.HostCallbackID, "")
	if errAcquire != nil {
		return nil, errAcquire
	}
	defer operation.finish()
	timer := time.NewTimer(time.Duration(req.Milliseconds) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-operation.ctx.Done():
	case <-timer.C:
	}
	return marshalRPCResult(pluginapi.HostContextWaitResponse{Canceled: operation.ctx.Err() != nil})
}

func (h *Host) callHostAuthQuota(_ context.Context, request []byte) ([]byte, error) {
	var req pluginapi.HostAuthGetRequest
	if errDecode := json.Unmarshal(request, &req); errDecode != nil {
		return nil, fmt.Errorf("decode auth quota: %w", errDecode)
	}
	auth, errAuth := h.authByIndex(req.AuthIndex)
	if errAuth != nil {
		return nil, errAuth
	}
	return marshalRPCResult(pluginapi.HostAuthQuotaResponse{
		AuthID: auth.ID, Provider: auth.Provider, AuthKind: auth.AuthKind(),
		Disabled:   auth.Disabled || auth.Status == coreauth.StatusDisabled,
		ObservedAt: auth.Quota.ObservedAt, Signals: auth.Quota.Clone().Signals,
	})
}
