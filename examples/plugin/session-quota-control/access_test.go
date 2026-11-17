package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

func TestAccessControlModesAndIntersection(t *testing.T) {
	for _, test := range []struct {
		name       string
		credential accessRule
		key        accessRule
		denied     string
	}{
		{name: "default empty blacklists"},
		{name: "explicit empty blacklists", credential: accessRule{Mode: accessBlacklist}, key: accessRule{Mode: accessBlacklist}},
		{name: "credential blacklist", credential: accessRule{Mode: accessBlacklist, Entries: []string{"client"}}, denied: "credential blacklist"},
		{name: "key blacklist", key: accessRule{Mode: accessBlacklist, Entries: []string{"account"}}, denied: "API key blacklist"},
		{name: "credential empty whitelist", credential: accessRule{Mode: accessWhitelist}, denied: "credential whitelist"},
		{name: "key empty whitelist", key: accessRule{Mode: accessWhitelist}, denied: "API key whitelist"},
		{name: "both whitelisted", credential: accessRule{Mode: accessWhitelist, Entries: []string{"client"}}, key: accessRule{Mode: accessWhitelist, Entries: []string{"account"}}},
		{name: "credential grant cannot override key denial", credential: accessRule{Mode: accessWhitelist, Entries: []string{"client"}}, key: accessRule{Mode: accessBlacklist, Entries: []string{"account"}}, denied: "API key blacklist"},
		{name: "key grant cannot override credential denial", credential: accessRule{Mode: accessBlacklist, Entries: []string{"client"}}, key: accessRule{Mode: accessWhitelist, Entries: []string{"account"}}, denied: "credential blacklist"},
		{name: "unrelated blacklist entries", credential: accessRule{Entries: []string{"other-key"}}, key: accessRule{Entries: []string{"other-account"}}},
		{name: "whitelist exact identity", credential: accessRule{Mode: accessWhitelist, Entries: []string{"Client"}}, denied: "credential whitelist"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := defaultAccessControl()
			cfg.CredentialRules["account"], cfg.APIKeyRules["client"] = test.credential, test.key
			reason := cfg.denial("client", "account")
			if (test.denied == "" && reason != "") || (test.denied != "" && !strings.Contains(reason, test.denied)) {
				t.Fatalf("denial = %q, want %q", reason, test.denied)
			}
		})
	}
}

func TestAccessControlConfigurationValidation(t *testing.T) {
	for _, mode := range []string{"", accessBlacklist, accessWhitelist, "off", "both", "allowlist"} {
		cfg := defaultAccessControl()
		cfg.CredentialRules["account"] = accessRule{Mode: mode}
		if err := cfg.validate(); (err == nil) != (mode == "" || mode == accessBlacklist || mode == accessWhitelist) {
			t.Fatalf("mode %q validation = %v", mode, err)
		}
	}
	for _, raw := range []string{
		"credential_rules:\n  account:\n    mode: whitelist\n    entries: []\n",
		"api_key_rules:\n  sk-client/with.dots:\n    mode: blacklist\n    entries: [account]\n",
	} {
		cfg := defaultAccessControl()
		if errDecode := yaml.Unmarshal([]byte(raw), &cfg); errDecode != nil {
			t.Fatal(errDecode)
		}
		if errValidate := cfg.validate(); errValidate != nil {
			t.Fatal(errValidate)
		}
		encoded, errEncode := yaml.Marshal(cfg)
		if errEncode != nil {
			t.Fatal(errEncode)
		}
		var restored accessControlConfig
		if errDecode := yaml.Unmarshal(encoded, &restored); errDecode != nil || restored.denial("sk-client/with.dots", "account") == "" {
			t.Fatalf("access restriction lost after round trip: %v", errDecode)
		}
	}
}

func TestAccessFilterUsesSpecificCredentialIndexAndRawClientKey(t *testing.T) {
	c, _ := testControl()
	c.cfg.AccessControl.CredentialRules["blocked-index"] = accessRule{Mode: accessWhitelist, Entries: []string{"sk-other"}}
	req := pluginapi.AuthCandidateFilterRequest{APIKey: "sk-client/full.key", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "blocked", AuthIndex: "blocked-index", Provider: "codex"}, {ID: "allowed", AuthIndex: "allowed-index", Provider: "codex"},
	}}
	raw, _ := json.Marshal(req)
	result, errHandle := c.handle(pluginabi.MethodAuthCandidatesFilter, raw)
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	response := result.(pluginapi.AuthCandidateFilterResponse)
	if len(response.Denied) != 1 || response.Denied["blocked"] == "" {
		t.Fatalf("credential filter = %+v", response)
	}
	c.cfg.AccessControl.APIKeyRules[req.APIKey] = accessRule{Mode: accessWhitelist}
	if response := c.filterAccess(req); len(response.Denied) != 2 {
		t.Fatalf("empty key whitelist did not deny all: %+v", response)
	}
}

func TestAccessStateReturnsFullAPIKeysAndNoUpstreamSecrets(t *testing.T) {
	c, _ := testControl()
	c.apiKeys = func() ([]string, error) { return []string{"sk-full-client-key/with.dots"}, nil }
	c.auths = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "account", AuthIndex: "index", Provider: "codex", Email: "user@example.com", Path: "/secret/path", BaseURL: "https://secret-upstream"}}, nil
	}
	response := c.accessState()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), "sk-full-client-key/with.dots") {
		t.Fatalf("full client key missing: status=%d", response.StatusCode)
	}
	if strings.Contains(string(response.Body), "secret") {
		t.Fatal("access catalog included upstream material")
	}
}

func TestManualAssignmentRechecksCurrentAccessRule(t *testing.T) {
	c, now := testControl()
	c.pending["pending"] = &pendingSession{ID: "pending", APIKey: "client", CreatedAt: *now,
		Candidates: []candidateView{{ID: "manual", AuthIndex: "index"}}}
	c.cfg.AccessControl.APIKeyRules["client"] = accessRule{Mode: accessBlacklist, Entries: []string{"index"}}
	response := c.assign([]byte(`{"id":"pending","auth_id":"manual"}`))
	if response.StatusCode != http.StatusForbidden || c.pending["pending"].released {
		t.Fatalf("unauthorized assignment: status=%d", response.StatusCode)
	}
}

func TestAccessChangeReleasesManualWait(t *testing.T) {
	c, _ := testControl()
	lookups := 0
	c.wait = func(_ string, _ time.Duration) error {
		lookups++
		c.mu.Lock()
		c.cfg.AccessControl.APIKeyRules["client"] = accessRule{Mode: accessWhitelist}
		c.mu.Unlock()
		return nil
	}
	response, errAwait := c.awaitAssignment(affinityCall{SessionAffinityRequest: pluginapi.SessionAffinityRequest{
		APIKey: "client", Provider: "codex", Model: "gpt", SessionID: "session",
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "manual", AuthIndex: "index"}},
	}})
	if errAwait != nil || response.AuthID != "" || lookups != 1 || len(c.pending) != 0 || c.history[0].Result != "access_denied" {
		t.Fatalf("wait was not released: response=%+v err=%v waits=%d", response, errAwait, lookups)
	}
}

func TestManualAssignmentQueuesAreIsolatedByAPIKey(t *testing.T) {
	c, _ := testControl()
	request := func(key string) affinityCall {
		return affinityCall{CallbackID: key, SessionAffinityRequest: pluginapi.SessionAffinityRequest{
			APIKey: key, Provider: "codex", Model: "gpt", SessionID: "same-session",
			Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "manual", AuthIndex: "index"}},
		}}
	}
	c.wait = func(key string, _ time.Duration) error {
		if key == "first-key" {
			response, errAwait := c.awaitAssignment(request("second-key"))
			if errAwait != nil || response.AuthID != "manual" {
				t.Fatalf("second queue=%+v error=%v", response, errAwait)
			}
			return nil
		}
		if len(c.pending) != 2 {
			t.Fatalf("different client keys merged into %d manual queue(s)", len(c.pending))
		}
		for _, pending := range c.pending {
			body, _ := json.Marshal(map[string]string{"id": pending.ID, "auth_id": "manual"})
			if response := c.assign(body); response.StatusCode != http.StatusOK {
				t.Fatalf("separate queue assignment status=%d", response.StatusCode)
			}
		}
		return nil
	}
	response, errAwait := c.awaitAssignment(request("first-key"))
	if errAwait != nil || response.AuthID != "manual" || len(c.pending) != 0 || len(c.history) != 2 {
		t.Fatalf("first queue=%+v error=%v pending=%d history=%d", response, errAwait, len(c.pending), len(c.history))
	}
}
