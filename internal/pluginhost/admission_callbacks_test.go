package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRoutingAuthListIncludesConfigurationCredentialsWithoutSecrets(t *testing.T) {
	host := New()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "config-key", Provider: "codex", Label: "Config credential", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "never-expose-this", "priority": "5"},
		Metadata:   map[string]any{"access_token": "never-expose-this-either"},
		Quota:      coreauth.QuotaState{ObservedAt: time.Now(), Signals: map[string]string{"X-Codex-Primary-Used-Percent": "20"}},
	}
	registered, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	host.SetAuthManager(manager)
	raw, errList := host.callHostAuthList(context.Background(), []byte(`{"include_runtime":true}`))
	if errList != nil {
		t.Fatal(errList)
	}
	var envelope pluginabi.Envelope
	if errDecode := json.Unmarshal(raw, &envelope); errDecode != nil {
		t.Fatal(errDecode)
	}
	var listing rpcHostAuthListResponse
	if errDecode := json.Unmarshal(envelope.Result, &listing); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(listing.Files) != 1 || listing.Files[0].ID != auth.ID || listing.Files[0].AuthIndex == "" {
		t.Fatalf("routing credentials = %+v", listing.Files)
	}
	if strings.Contains(string(raw), "never-expose") {
		t.Fatal("routing list exposed credential secrets")
	}
	quotaRequest, _ := json.Marshal(pluginapi.HostAuthGetRequest{AuthIndex: registered.EnsureIndex()})
	quota, errQuota := host.callHostAuthQuota(context.Background(), quotaRequest)
	if errQuota != nil || strings.Contains(string(quota), "never-expose") {
		t.Fatalf("quota leaked secrets or failed: %v", errQuota)
	}
	if !strings.Contains(string(quota), "apikey") {
		t.Fatalf("quota omitted credential classification: %s", quota)
	}
}

func TestContextWaitObservesCancellationAndCallbackOwnership(t *testing.T) {
	host := New()
	ctx, cancel := context.WithCancel(context.Background())
	callbackID, closeCallback := host.openCallbackContextForPlugin(ctx, "owner")
	defer closeCallback()
	cancel()
	raw, _ := json.Marshal(pluginapi.HostContextWaitRequest{HostCallbackID: callbackID, Milliseconds: 3600000})
	response, errWait := host.callHostContextWait(withHostCallbackPluginID(context.Background(), "owner"), raw)
	if errWait != nil {
		t.Fatal(errWait)
	}
	var envelope pluginabi.Envelope
	if errDecode := json.Unmarshal(response, &envelope); errDecode != nil {
		t.Fatal(errDecode)
	}
	var result pluginapi.HostContextWaitResponse
	if errDecode := json.Unmarshal(envelope.Result, &result); errDecode != nil {
		t.Fatal(errDecode)
	}
	if !result.Canceled {
		t.Fatal("canceled request kept waiting")
	}
	if _, errWait := host.callHostContextWait(withHostCallbackPluginID(context.Background(), "other"), raw); errWait == nil {
		t.Fatal("another plugin accessed the request context")
	}
}

type v8ExtensionFixture struct{}

func (v8ExtensionFixture) RegisterManagement(_ context.Context, req pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	return pluginapi.ManagementRegistrationResponse{ExtensionRoutes: []pluginapi.ManagementRoute{
		{Method: http.MethodPost, Path: "/assign", Handler: managementHandlerFunc(func(_ context.Context, call pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
			return pluginapi.ManagementResponse{Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"label":"<manual>"}`)}, nil
		})},
	}}, nil
}

func TestV8PluginExtensionsStayInsidePluginNamespace(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{id: "admission", plugin: pluginapi.Plugin{SchemaVersion: pluginabi.SchemaVersion, Capabilities: pluginapi.Capabilities{ManagementAPI: v8ExtensionFixture{}}}})
	host.RegisterManagementRoutes(context.Background(), nil)
	response := httptest.NewRecorder()
	if !host.ServeManagementHTTP(response, httptest.NewRequest(http.MethodPost, "/v8/management/plugins/admission/extension/assign", strings.NewReader(`{}`))) {
		t.Fatal("v8 extension route was not registered")
	}
	if response.Code != http.StatusOK || response.Body.String() != `{"label":"<manual>"}` {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	for _, path := range []string{"../other", "/../other", "/state?secret=1", "/v8/management/plugins/other/extension/state"} {
		_, _, valid := normalizeExtensionRoute("admission", pluginapi.ManagementRoute{Path: path, Handler: managementHandlerFunc(func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
			return pluginapi.ManagementResponse{}, nil
		})})
		if valid {
			t.Fatalf("accepted extension path %q", path)
		}
	}
	if host.ServeManagementHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v0/management/plugins/admission/extension/assign", nil)) {
		t.Fatal("v8 extension was exposed under deprecated management routes")
	}
}
