package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/examples/plugin/session-quota-control/evaluation"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type hubFixture struct {
	mu            sync.Mutex
	profiles      hubProfiles
	calls         map[string]int
	writes        [][]int64
	failWrite     int
	deliverFailed bool
}

func originalHubProfiles() hubProfiles {
	return hubProfiles{Active: "primary", Profiles: []map[string]any{
		{"name": "primary", "channelBindingMode": "dynamic", "channelIDs": []int64{91, 92}, "channelTags": []string{"fast"},
			"dynamicChannelStrategy": map[string]any{"mode": "auto", "fallbackChannelIDs": []int64{99}},
			"routingPolicy":          map[string]any{"fallbackChannelIDs": []int64{98}}, "quota": map[string]any{"requests": 1000},
			"modelMappings": []any{map[string]any{"from": "alias", "to": "model"}}},
		{"name": "secondary", "channelIDs": []int64{77}, "channelBindingMode": "manual"},
	}}
}

func newHubFixture(t *testing.T) (*hubFixture, *httptest.Server) {
	t.Helper()
	f := &hubFixture{profiles: originalHubProfiles(), calls: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hub-private-token" {
			t.Error("missing Hub authorization")
		}
		if r.Header.Get("X-Project-ID") != "gid://axonhub/Project/1" {
			t.Error("missing Hub project")
		}
		var request struct {
			Operation string                     `json:"operationName"`
			Variables map[string]json.RawMessage `json:"variables"`
		}
		if errDecode := json.NewDecoder(r.Body).Decode(&request); errDecode != nil {
			t.Error(errDecode)
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls[request.Operation]++
		var data any
		switch request.Operation {
		case "GetApiKey":
			data = map[string]any{"node": hubKey{ID: "gid://axonhub/APIKey/825", Key: "sk-private-model-key", Profiles: f.profiles}}
		case "UpdateAPIKeyProfiles":
			var next hubProfiles
			if errDecode := json.Unmarshal(request.Variables["input"], &next); errDecode != nil {
				t.Error(errDecode)
			}
			f.writes = append(f.writes, next.channelIDs())
			if len(f.writes) == f.failWrite {
				if f.deliverFailed {
					f.profiles = next
				}
				w.WriteHeader(502)
				return
			}
			f.profiles = next
			data = map[string]any{"updateAPIKeyProfiles": hubKey{ID: "gid://axonhub/APIKey/825", Profiles: next}}
		case "FreeChannels":
			page := f.calls[request.Operation]
			if page == 2 && string(request.Variables["after"]) != `"next"` {
				t.Error("pagination cursor was lost")
			}
			free := func(id int, price any) map[string]any {
				return map[string]any{"node": map[string]any{"id": fmt.Sprintf("gid://axonhub/Channel/%d", id), "name": fmt.Sprint(id), "supportedModels": []string{"gpt-6-astra"}, "channelModelPrices": []any{map[string]any{"modelID": "gpt-6-astra", "price": map[string]any{"items": []any{map[string]any{"pricing": map[string]any{"flatFee": price, "usagePerUnit": price}}}}}}}}
			}
			edges := []any{free(11, 0), free(12, 1), free(13, nil)}
			if page == 2 {
				edges = []any{free(21, 0)}
			}
			data = map[string]any{"channels": map[string]any{"edges": edges, "pageInfo": map[string]any{"hasNextPage": page == 1, "endCursor": "next"}}}
		default:
			t.Errorf("unexpected operation %s", request.Operation)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if errEncode := json.NewEncoder(w).Encode(map[string]any{"data": data}); errEncode != nil {
			t.Error(errEncode)
		}
	}))
	t.Cleanup(server.Close)
	return f, server
}

func hubTestManager(t *testing.T, server *httptest.Server) *hubManager {
	t.Helper()
	s := newHubManager()
	s.settle = func(ctx context.Context) error { return ctx.Err() }
	if errConfigure := s.configure(filepath.Join(t.TempDir(), "hub.json")); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	t.Cleanup(s.stop)
	settings := defaultHubSettings()
	settings.Authorization = "hub-private-token"
	settings.KeyID = 825
	settings.GraphQLURL = server.URL
	settings.APIBaseURL = server.URL
	settings.ProxyURL = "direct"
	raw, _ := json.Marshal(settings)
	if errSave := s.settings(raw); errSave != nil {
		t.Fatal(errSave)
	}
	return s
}

func fixtureHubResult(t *testing.T, match bool, used int) evaluation.Result {
	t.Helper()
	var result evaluation.Result
	raw := fmt.Sprintf(`{"status":"completed","done":3,"total":3,"model_match":%t,"attribution":{"used_outputs":%d,"prediction":"gpt-6-astra","decision":"model"}}`, match, used)
	if errDecode := json.Unmarshal([]byte(raw), &result); errDecode != nil {
		t.Fatal(errDecode)
	}
	return result
}

func TestHubSettingsPersistWithoutReturningSecrets(t *testing.T) {
	_, server := newHubFixture(t)
	s := hubTestManager(t, server)
	if errSave := s.settings([]byte(`{"cookie":"private-cookie","model":"custom-model","filter":"custom","effort":"high","channel_ids":[11,21]}`)); errSave != nil {
		t.Fatal(errSave)
	}
	if errSave := s.settings([]byte(`{"authorization":"","cookie":""}`)); errSave != nil {
		t.Fatal(errSave)
	}
	path := s.path
	s.stop()
	reopened := newHubManager()
	t.Cleanup(reopened.stop)
	if errConfigure := reopened.configure(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	settings := reopened.disk.Settings
	if settings.Authorization != "Bearer hub-private-token" || settings.Cookie != "private-cookie" || settings.Model != "custom-model" || settings.Filter != "custom" || settings.Effort != "high" || !slices.Equal(settings.ChannelIDs, []int64{11, 21}) {
		t.Fatalf("settings did not survive restart: %+v", settings.public())
	}
	public := string(reopened.state().Body)
	if strings.Contains(public, "private-token") || strings.Contains(public, "private-cookie") || !strings.Contains(public, `"authorization_saved":true`) {
		t.Fatal("saved secrets were returned to the page")
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatal(errStat)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode=%v", info.Mode())
	}
}

func TestHubDiscoveryDoesNotRequireKeyOrRunModels(t *testing.T) {
	f, server := newHubFixture(t)
	s := hubTestManager(t, server)
	if errSave := s.settings([]byte(`{"key_id":0,"model":""}`)); errSave != nil {
		t.Fatal(errSave)
	}
	s.run = func(context.Context, *hubClient, string, evaluation.Options, func(evaluation.Result)) evaluation.Result {
		t.Error("discovery ran a model")
		return evaluation.Result{}
	}
	if errLaunch := s.launch("discover", nil, ""); errLaunch != nil {
		t.Fatal(errLaunch)
	}
	s.workers.Wait()
	if len(s.disk.Channels) != 2 || s.disk.Channels[0].ID != 11 || s.disk.Channels[1].ID != 21 {
		t.Fatalf("free channels=%+v", s.disk.Channels)
	}
	if f.calls["GetApiKey"] != 0 || len(f.writes) != 0 || len(s.disk.Jobs) != 0 {
		t.Fatal("discovery changed a key or started detection")
	}
	if f.calls["FreeChannels"] != 2 {
		t.Fatal("discovery did not paginate")
	}
}

func TestHubDetectionRestoresThenManualApplyUsesOnlyPassingChannels(t *testing.T) {
	f, server := newHubFixture(t)
	s := hubTestManager(t, server)
	runs := 0
	s.run = func(ctx context.Context, c *hubClient, secret string, options evaluation.Options, progress func(evaluation.Result)) evaluation.Result {
		runs++
		f.mu.Lock()
		defer f.mu.Unlock()
		if secret != "sk-private-model-key" || options.Model != "gpt-6-astra" || options.Effort != "low" || options.Test != "fingerprint" {
			t.Error("wrong detection input")
		}
		if len(f.profiles.channelIDs()) != 1 || f.profiles.channelIDs()[0] != int64(runs) {
			t.Error("detection was not pinned to the current channel")
		}
		profile := f.profiles.Profiles[0]
		if len(profile["routingPolicy"].(map[string]any)["fallbackChannelIDs"].([]any)) != 0 || profile["dynamicChannelStrategy"] != nil || len(profile["channelTags"].([]any)) != 0 {
			t.Error("alternate routes were not cleared")
		}
		if runs == 3 {
			return fixtureHubResult(t, true, 2)
		}
		return fixtureHubResult(t, runs == 1, 3)
	}
	if errLaunch := s.launch("detect", []int64{1, 2, 3}, ""); errLaunch != nil {
		t.Fatal(errLaunch)
	}
	s.workers.Wait()
	if f.calls["FreeChannels"] != 0 || runs != 3 {
		t.Fatal("detection depended on discovery")
	}
	if !equalHubProfiles(f.profiles, originalHubProfiles()) || s.disk.Recovery != nil {
		t.Fatal("detection did not restore the complete original profile")
	}
	job := s.disk.Jobs[0]
	if job.Status != "completed" || !job.AppliedAt.IsZero() {
		t.Fatalf("job=%+v", job)
	}
	if errLaunch := s.launch("apply", nil, job.ID); errLaunch != nil {
		t.Fatal(errLaunch)
	}
	s.workers.Wait()
	if !slices.Equal(f.profiles.channelIDs(), []int64{1}) || job.AppliedAt.IsZero() {
		t.Fatalf("manual apply selected %v", f.profiles.channelIDs())
	}
	if !equalHubProfiles(hubProfiles{Profiles: f.profiles.Profiles[1:]}, hubProfiles{Profiles: originalHubProfiles().Profiles[1:]}) {
		t.Fatal("inactive profile changed")
	}
	if errSave := s.settings([]byte(`{"key_id":826}`)); errSave != nil {
		t.Fatal(errSave)
	}
	if errLaunch := s.launch("apply", nil, job.ID); errLaunch == nil {
		t.Fatal("applied a result to another Key")
	}
}

func TestHubCancellationRestoresAndPreservesSavedSelection(t *testing.T) {
	f, server := newHubFixture(t)
	s := hubTestManager(t, server)
	started := make(chan struct{})
	s.run = func(ctx context.Context, _ *hubClient, _ string, _ evaluation.Options, _ func(evaluation.Result)) evaluation.Result {
		close(started)
		<-ctx.Done()
		return evaluation.Result{Status: "cancelled"}
	}
	if errLaunch := s.launch("detect", []int64{1, 2}, ""); errLaunch != nil {
		t.Fatal(errLaunch)
	}
	<-started
	if errLaunch := s.launch("discover", nil, ""); errLaunch == nil {
		t.Fatal("overlapping operation accepted")
	}
	s.cancelJob()
	s.workers.Wait()
	if !equalHubProfiles(f.profiles, originalHubProfiles()) || s.disk.Recovery != nil || s.disk.Jobs[0].Status != "cancelled" {
		t.Fatal("cancellation did not restore binding")
	}
	if !slices.Equal(s.disk.Settings.ChannelIDs, []int64{1, 2}) {
		t.Fatal("selection not saved")
	}
}

func TestHubLostMutationResponseRestoresEitherPossibleBinding(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		t.Run(fmt.Sprint(delivered), func(t *testing.T) {
			f, server := newHubFixture(t)
			s := hubTestManager(t, server)
			f.failWrite = 2
			f.deliverFailed = delivered
			s.run = func(context.Context, *hubClient, string, evaluation.Options, func(evaluation.Result)) evaluation.Result {
				return fixtureHubResult(t, true, 3)
			}
			if errLaunch := s.launch("detect", []int64{1, 2}, ""); errLaunch != nil {
				t.Fatal(errLaunch)
			}
			s.workers.Wait()
			if s.disk.Recovery != nil || !equalHubProfiles(f.profiles, originalHubProfiles()) || s.disk.Jobs[0].Status != "failed" {
				t.Fatal("ambiguous mutation outcome was not recovered")
			}
		})
	}
}

func TestHubRestartCanRecoverInterruptedBinding(t *testing.T) {
	f, server := newHubFixture(t)
	s := hubTestManager(t, server)
	started := make(chan struct{})
	s.run = func(ctx context.Context, _ *hubClient, _ string, _ evaluation.Options, _ func(evaluation.Result)) evaluation.Result {
		close(started)
		<-ctx.Done()
		return evaluation.Result{Status: "cancelled"}
	}
	if errLaunch := s.launch("detect", []int64{1}, ""); errLaunch != nil {
		t.Fatal(errLaunch)
	}
	<-started
	s.stop()
	if s.disk.Recovery == nil {
		t.Fatal("shutdown discarded the recovery journal")
	}
	reopened := newHubManager()
	t.Cleanup(reopened.stop)
	if errConfigure := reopened.configure(s.path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	if errSave := reopened.settings([]byte(`{"key_id":826}`)); errSave == nil {
		t.Fatal("changed recovery target")
	}
	if errLaunch := reopened.launch("detect", []int64{2}, ""); errLaunch == nil {
		t.Fatal("new detection started before recovery")
	}
	if errLaunch := reopened.launch("recover", nil, ""); errLaunch != nil {
		t.Fatal(errLaunch)
	}
	reopened.workers.Wait()
	if reopened.disk.Recovery != nil || !equalHubProfiles(f.profiles, originalHubProfiles()) {
		t.Fatal("restart recovery failed")
	}
}

func TestHubDoesNotOverwriteExternalProfileChanges(t *testing.T) {
	f, server := newHubFixture(t)
	s := hubTestManager(t, server)
	external := originalHubProfiles().bound([]int64{123})
	s.run = func(context.Context, *hubClient, string, evaluation.Options, func(evaluation.Result)) evaluation.Result {
		f.mu.Lock()
		f.profiles = external
		f.mu.Unlock()
		return fixtureHubResult(t, true, 3)
	}
	if errLaunch := s.launch("detect", []int64{1}, ""); errLaunch != nil {
		t.Fatal(errLaunch)
	}
	s.workers.Wait()
	if !equalHubProfiles(f.profiles, external) || s.disk.Recovery == nil {
		t.Fatal("external changes were overwritten")
	}
}

func TestHubDashboardAndExtensionRoutes(t *testing.T) {
	c, _ := testControl()
	t.Cleanup(c.stop)
	c.extensionBase = "/v8/management/plugins/session-quota-control/extension"
	c.resourceBase = "/plugins/session-quota-control/resources"
	response := c.serve(pluginapi.ManagementRequest{Method: "GET", Path: c.resourceBase + "/dashboard"})
	page := string(response.Body)
	for _, text := range []string{`id="hubPanel"`, `id="hubDiscover"`, `id="hubDetect"`, `id="hubSettings"`, `const hubView=`} {
		if !strings.Contains(page, text) {
			t.Errorf("page missing %s", text)
		}
	}
	if strings.Contains(page, "__HUB_") {
		t.Fatal("unexpanded Hub content")
	}
	if got := c.serve(pluginapi.ManagementRequest{Method: "GET", Path: c.extensionBase + "/hub/state"}); got.StatusCode != 200 {
		t.Fatalf("state route=%d", got.StatusCode)
	}
	if got := c.serve(pluginapi.ManagementRequest{Method: "GET", Path: c.extensionBase + "/hub/apply"}); got.StatusCode != 404 {
		t.Fatal("GET could mutate channels")
	}
}

func TestHubProtocolResponsesAndMessages(t *testing.T) {
	for _, protocol := range []string{"codex", "claude"} {
		t.Run(protocol, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer model-secret" {
					t.Error("wrong model authorization")
				}
				var body map[string]any
				if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
					t.Error(errDecode)
				}
				if body["model"] != "requested-model" || body["stream"] != true {
					t.Error("model payload lost")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if protocol == "codex" {
					if r.URL.Path != "/v1/responses" || body["reasoning"].(map[string]any)["effort"] != "high" {
						t.Error("wrong Responses request")
					}
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"1, 2, 3\"}]}],\"usage\":{\"input_tokens\":12,\"output_tokens\":3}}}\n\n")
				} else {
					if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "model-secret" || body["output_config"].(map[string]any)["effort"] != "high" {
						t.Error("wrong Messages request")
					}
					fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12}}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"1, 2, 3\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
				}
			}))
			defer server.Close()
			settings := defaultHubSettings()
			settings.APIBaseURL = server.URL
			settings.Protocol = protocol
			settings.ProxyURL = "direct"
			client, errClient := newHubClient(settings)
			if errClient != nil {
				t.Fatal(errClient)
			}
			defer client.http.CloseIdleConnections()
			out, errProbe := client.probe("model-secret")(t.Context(), evaluation.ProbeRequest{Model: "requested-model", Effort: "high", Prompt: "challenge", Instructions: "instructions"})
			if errProbe != nil || out.Text != "1, 2, 3" || out.InputTokens != 12 || out.OutputTokens != 3 {
				t.Fatalf("out=%+v error=%v", out, errProbe)
			}
		})
	}
}

func TestHubRejectsIncompleteOrErrorStreams(t *testing.T) {
	for _, stream := range []string{
		"data: {\"type\":\"error\",\"error\":{\"message\":\"secret\"}}\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"1,2,3\"}\n\n",
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"incomplete\"}}\n\n",
		"data: invalid\n\n",
	} {
		if _, errRead := readHubProbe(strings.NewReader(stream), true, false); errRead == nil {
			t.Fatal("bad stream counted as available")
		}
	}
	input := evaluation.ProbeRequest{Model: "claude-sonnet-5", Effort: "none", Prompt: "test"}
	body, _, path := hubProbePayload("auto", input)
	if path != "/messages?beta=true" || body["thinking"] != nil {
		t.Fatal("auto protocol/default effort failed")
	}
	if !reflect.DeepEqual(body["messages"], []any{map[string]any{"role": "user", "content": "test"}}) {
		t.Fatal("probe prompt changed")
	}
}

func TestHubProbeCancellationClosesStreamWithoutTimeout(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer server.Close()
	settings := defaultHubSettings()
	settings.APIBaseURL, settings.ProxyURL = server.URL, "direct"
	client, errClient := newHubClient(settings)
	if errClient != nil {
		t.Fatal(errClient)
	}
	defer client.http.CloseIdleConnections()
	if client.http.Timeout != 0 || client.http.Transport.(*http.Transport).ResponseHeaderTimeout != 0 {
		t.Fatal("Hub added a request timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, errProbe := client.probe("model-secret")(ctx, evaluation.ProbeRequest{Model: "model", Prompt: "test"})
		finished <- errProbe
	}()
	<-started
	cancel()
	if errProbe := <-finished; errProbe == nil {
		t.Fatal("cancelled stream counted as success")
	}
	<-stopped
}

func TestHubSecretsAreNotForwardedOnRedirect(t *testing.T) {
	forwarded := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true; w.WriteHeader(200) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	settings := defaultHubSettings()
	settings.APIBaseURL, settings.ProxyURL = server.URL, "direct"
	client, errClient := newHubClient(settings)
	if errClient != nil {
		t.Fatal(errClient)
	}
	defer client.http.CloseIdleConnections()
	if _, errProbe := client.probe("model-secret")(t.Context(), evaluation.ProbeRequest{Model: "model", Prompt: "test"}); errProbe == nil || forwarded {
		t.Fatal("redirect followed with model credentials")
	}
}
