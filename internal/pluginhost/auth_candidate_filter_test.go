package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type authCandidateFilterFunc func(context.Context, pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error)

func (fn authCandidateFilterFunc) FilterAuthCandidates(ctx context.Context, req pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
	return fn(ctx, req)
}

func TestHostCredentialFilterOnlyPluginIsValid(t *testing.T) {
	plugin := validTestPlugin("filter")
	plugin.Capabilities = pluginapi.Capabilities{AuthCandidateFilter: authCandidateFilterFunc(func(context.Context, pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
		return pluginapi.AuthCandidateFilterResponse{}, nil
	})}
	if !validPlugin(plugin) {
		t.Fatal("credential filter alone must be a valid plugin capability")
	}
}

func TestHostCredentialFilterSurvivesFailedReconfiguration(t *testing.T) {
	for _, mode := range []string{"invalid", "panic"} {
		t.Run(mode, func(t *testing.T) {
			plugin := validTestPlugin("filter")
			plugin.Capabilities = pluginapi.Capabilities{AuthCandidateFilter: authCandidateFilterFunc(func(context.Context, pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
				return pluginapi.AuthCandidateFilterResponse{Denied: map[string]string{"account": "blocked"}}, nil
			})}
			loader := newTestSymbolLoader()
			loader.lookups["filter"] = newTestSymbolLookup(&testPlugin{registerResult: plugin, panicOnReload: mode == "panic"})
			host := NewForTest(loader)
			t.Cleanup(host.ShutdownAll)
			cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: makePluginDir(t, "filter"), Configs: enabledPluginConfigs("filter")}}
			host.ApplyConfig(context.Background(), cfg)
			req := pluginapi.AuthCandidateFilterRequest{APIKey: "client", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "account"}}}
			response, errFilter := host.FilterAuthCandidates(context.Background(), req)
			if errFilter != nil || response.Denied["account"] != "blocked" {
				t.Fatalf("initial filter not enforced: response=%+v error=%v", response, errFilter)
			}
			for range 2 {
				host.ApplyConfig(context.Background(), cfg)
				if !host.HasAuthCandidateFilters() {
					t.Fatal("failed reconfiguration removed access enforcement")
				}
				response, errFilter = host.FilterAuthCandidates(context.Background(), req)
				if errFilter == nil && response.Denied["account"] == "" {
					t.Fatal("failed reconfiguration allowed an unfiltered request")
				}
			}
		})
	}
}

func TestHostCredentialFiltersCombineDenials(t *testing.T) {
	records := make([]capabilityRecord, 0, 2)
	for _, id := range []string{"first", "second"} {
		records = append(records, capabilityRecord{id: id, meta: pluginapi.Metadata{Name: id}, plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			AuthCandidateFilter: authCandidateFilterFunc(func(_ context.Context, req pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
				if req.Plugin.Name != id || req.APIKey != "full-client-key" {
					t.Fatal("filter request lost plugin or client identity")
				}
				return pluginapi.AuthCandidateFilterResponse{Denied: map[string]string{id: "denied"}}, nil
			}),
		}}})
	}
	host := newHostWithRecords(records...)
	response, errFilter := host.FilterAuthCandidates(context.Background(), pluginapi.AuthCandidateFilterRequest{APIKey: "full-client-key",
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "first"}, {ID: "second"}, {ID: "third"}}})
	if errFilter != nil || len(response.Denied) != 2 || response.Denied["first"] == "" || response.Denied["second"] == "" {
		t.Fatalf("combined filter=%+v error=%v", response, errFilter)
	}
}

func TestHostCredentialFilterStaleSnapshotFailsClosed(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{id: "filter", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
		AuthCandidateFilter: authCandidateFilterFunc(func(context.Context, pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
			t.Fatal("stale filter must not be invoked")
			return pluginapi.AuthCandidateFilterResponse{}, nil
		}),
	}}})
	host.mu.Lock()
	host.rebuildActivePluginMapsLocked(nil)
	host.mu.Unlock()
	if !host.HasAuthCandidateFilters() {
		t.Fatal("snapshot replacement temporarily removed access enforcement")
	}
	_, errFilter := host.FilterAuthCandidates(context.Background(), pluginapi.AuthCandidateFilterRequest{
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "account"}},
	})
	if errFilter == nil {
		t.Fatal("stale snapshot allowed an unfiltered request")
	}
}

func TestHostCredentialFilterErrorsAndPanicsFailClosed(t *testing.T) {
	for _, mode := range []string{"error", "panic", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			host := newHostWithRecords(capabilityRecord{id: "filter", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
				AuthCandidateFilter: authCandidateFilterFunc(func(context.Context, pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
					if mode == "panic" {
						panic("test filter failed")
					}
					if mode == "error" {
						return pluginapi.AuthCandidateFilterResponse{}, errors.New("filter unavailable")
					}
					return pluginapi.AuthCandidateFilterResponse{Denied: map[string]string{"unknown": "blocked"}}, nil
				}),
			}}})
			for range 2 {
				if !host.HasAuthCandidateFilters() {
					t.Fatal("failed filter disappeared from enforcement")
				}
				_, errFilter := host.FilterAuthCandidates(context.Background(), pluginapi.AuthCandidateFilterRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "account"}}})
				if errFilter == nil {
					t.Fatal("filter failure allowed unfiltered fallback")
				}
			}
		})
	}
}

func TestOwnAdministrativeModelChecksSkipOnlyKeylessOriginFilter(t *testing.T) {
	calls := 0
	host := newHostWithRecords(capabilityRecord{id: "origin", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
		AuthCandidateFilter: authCandidateFilterFunc(func(context.Context, pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
			calls++
			return pluginapi.AuthCandidateFilterResponse{Denied: map[string]string{"account": "blocked"}}, nil
		}),
	}}})
	for _, test := range []struct {
		origin string
		apiKey string
		denied bool
	}{
		{origin: "origin"}, {origin: "origin", apiKey: "client", denied: true}, {origin: "other", denied: true}, {denied: true},
	} {
		response, errFilter := host.FilterAuthCandidates(withHostCallbackPluginID(context.Background(), test.origin), pluginapi.AuthCandidateFilterRequest{
			APIKey: test.apiKey, Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "account"}}})
		if errFilter != nil || (len(response.Denied) != 0) != test.denied {
			t.Fatalf("administrative filtering decision incorrect: %v", errFilter)
		}
	}
	if calls != 3 {
		t.Fatalf("filter calls=%d, want 3", calls)
	}
}

func TestHostAPIKeyCatalogReturnsOnlyCompleteDownstreamKeys(t *testing.T) {
	host := New()
	host.runtimeConfig = &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"sk-client/full.key"}},
		CodexKey: []config.CodexKey{{APIKey: "upstream-secret"}}}
	raw, errList := host.callFromPlugin(context.Background(), pluginabi.MethodHostAPIKeysList, []byte(`{}`))
	if errList != nil {
		t.Fatal(errList)
	}
	var envelope pluginabi.Envelope
	if errDecode := json.Unmarshal(raw, &envelope); errDecode != nil {
		t.Fatal(errDecode)
	}
	var response pluginapi.HostAPIKeysResponse
	if errDecode := json.Unmarshal(envelope.Result, &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(response.APIKeys) != 1 || response.APIKeys[0] != "sk-client/full.key" {
		t.Fatal("catalog changed or masked the downstream key")
	}
}
