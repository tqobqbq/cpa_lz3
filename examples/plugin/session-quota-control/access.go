package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	accessBlacklist = "blacklist"
	accessWhitelist = "whitelist"
)

type accessRule struct {
	Mode    string   `json:"mode" yaml:"mode"`
	Entries []string `json:"entries" yaml:"entries"`
}

type accessControlConfig struct {
	CredentialRules map[string]accessRule `json:"credential_rules" yaml:"credential_rules"`
	APIKeyRules     map[string]accessRule `json:"api_key_rules" yaml:"api_key_rules"`
}

func defaultAccessControl() accessControlConfig {
	return accessControlConfig{CredentialRules: make(map[string]accessRule), APIKeyRules: make(map[string]accessRule)}
}

func (cfg accessControlConfig) validate() error {
	for _, rules := range []map[string]accessRule{cfg.CredentialRules, cfg.APIKeyRules} {
		for id, rule := range rules {
			if strings.TrimSpace(id) == "" || strings.TrimSpace(id) != id {
				return fmt.Errorf("access_control rule identifiers must be nonempty and have no surrounding whitespace")
			}
			if rule.Mode != "" && rule.Mode != accessBlacklist && rule.Mode != accessWhitelist {
				return fmt.Errorf("access_control mode must be blacklist or whitelist")
			}
			for _, entry := range rule.Entries {
				if strings.TrimSpace(entry) == "" || strings.TrimSpace(entry) != entry {
					return fmt.Errorf("access_control entries must be nonempty and have no surrounding whitespace")
				}
			}
		}
	}
	return nil
}

func (rule accessRule) allows(value string) bool {
	listed := slices.Contains(rule.Entries, value)
	if rule.Mode == accessWhitelist {
		return listed
	}
	return !listed
}

func (cfg accessControlConfig) denial(apiKey, index string) string {
	credentialRule := cfg.CredentialRules[index]
	if !credentialRule.allows(apiKey) {
		if credentialRule.Mode == accessWhitelist {
			return "API key is not in the credential whitelist"
		}
		return "API key is in the credential blacklist"
	}
	keyRule := cfg.APIKeyRules[apiKey]
	if !keyRule.allows(index) {
		if keyRule.Mode == accessWhitelist {
			return "Credential is not in the API key whitelist"
		}
		return "Credential is in the API key blacklist"
	}
	return ""
}

func (c *control) filterAccess(req pluginapi.AuthCandidateFilterRequest) pluginapi.AuthCandidateFilterResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	response := pluginapi.AuthCandidateFilterResponse{Denied: make(map[string]string)}
	for _, candidate := range req.Candidates {
		if reason := c.cfg.AccessControl.denial(req.APIKey, candidate.AuthIndex); reason != "" {
			response.Denied[candidate.ID] = reason
		}
	}
	return response
}

func (c *control) accessState() pluginapi.ManagementResponse {
	credentials, errCredentials := c.auths()
	if errCredentials != nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "cannot read credentials"})
	}
	keys, errKeys := c.apiKeys()
	if errKeys != nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "cannot read API keys"})
	}
	// Only account identity is needed here, never upstream credential material.
	entries := make([]map[string]any, 0, len(credentials))
	for _, credential := range credentials {
		entries = append(entries, map[string]any{"id": credential.ID, "auth_index": credential.AuthIndex,
			"provider": credential.Provider, "name": credential.Name, "label": credential.Label,
			"email": credential.Email, "account": credential.Account, "disabled": credential.Disabled})
	}
	c.mu.Lock()
	cfg := c.cfg.AccessControl
	c.mu.Unlock()
	return jsonResponse(http.StatusOK, map[string]any{"config": cfg, "api_keys": keys, "credentials": entries})
}
