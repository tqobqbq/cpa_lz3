package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type config struct {
	ManualAssignment  bool                      `json:"manual_assignment_enabled" yaml:"manual_assignment_enabled"`
	AssignmentWait    int                       `json:"assignment_wait_seconds" yaml:"assignment_wait_seconds"`
	QuotaEnabled      bool                      `json:"quota_pacing_enabled" yaml:"quota_pacing_enabled"`
	SafetyMargin      float64                   `json:"safety_margin_percent" yaml:"safety_margin_percent"`
	InitialCost       float64                   `json:"initial_request_percent" yaml:"initial_request_percent"`
	ModelSamples      int                       `json:"model_average_samples" yaml:"model_average_samples"`
	ConcurrencyWindow int                       `json:"concurrency_window_seconds" yaml:"concurrency_window_seconds"`
	MaxPending        int                       `json:"max_pending" yaml:"max_pending"`
	CredentialWaits   map[string]credentialWait `json:"credential_waits" yaml:"credential_waits"`
	UsageEnabled      bool                      `json:"usage_report_enabled" yaml:"usage_report_enabled"`
	UsageDBPath       string                    `json:"usage_db_path" yaml:"usage_db_path"`
	EvaluationDBPath  string                    `json:"evaluation_db_path" yaml:"evaluation_db_path"`
	AccessControl     accessControlConfig       `json:"access_control" yaml:"access_control"`
}

type credentialWait struct {
	MaxWaitSeconds   *int `json:"max_wait_seconds" yaml:"max_wait_seconds"`
	FixedWaitSeconds *int `json:"fixed_wait_seconds" yaml:"fixed_wait_seconds"`
}

func defaultConfig() config {
	return config{ManualAssignment: true, AssignmentWait: 60, QuotaEnabled: true,
		SafetyMargin: 5, InitialCost: 1, ModelSamples: 100, ConcurrencyWindow: 300,
		MaxPending: 1024, CredentialWaits: make(map[string]credentialWait),
		UsageEnabled: true, UsageDBPath: "~/.cli-proxy-api/session-quota-control/usage.sqlite",
		EvaluationDBPath: "~/.cli-proxy-api/session-quota-control/evaluations.sqlite", AccessControl: defaultAccessControl()}
}

func (cfg config) credentialWait(index, authID string) credentialWait {
	if rule, exists := cfg.CredentialWaits[index]; exists {
		return rule
	}
	return cfg.CredentialWaits[authID]
}

func (cfg config) validate() error {
	if errAccess := cfg.AccessControl.validate(); errAccess != nil {
		return errAccess
	}
	if strings.TrimSpace(cfg.EvaluationDBPath) == "" {
		return fmt.Errorf("evaluation_db_path cannot be empty")
	}
	if cfg.UsageEnabled && strings.TrimSpace(cfg.UsageDBPath) == "" {
		return fmt.Errorf("usage_db_path cannot be empty when usage_report_enabled is true")
	}
	for _, value := range []float64{cfg.SafetyMargin, cfg.InitialCost} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("numeric configuration must be finite")
		}
	}
	if cfg.AssignmentWait < 0 || cfg.AssignmentWait > 3600 {
		return fmt.Errorf("assignment_wait_seconds must be between 0 and 3600")
	}
	if cfg.SafetyMargin < 0 || cfg.SafetyMargin >= 100 || cfg.InitialCost <= 0 || cfg.InitialCost > 100 {
		return fmt.Errorf("invalid safety margin or initial request cost")
	}
	if cfg.ModelSamples < 1 || cfg.ModelSamples > 10000 || cfg.ConcurrencyWindow < 1 || cfg.ConcurrencyWindow > 3600 || cfg.MaxPending < 1 || cfg.MaxPending > 10000 {
		return fmt.Errorf("invalid moving average window or pending request limit")
	}
	for key, rule := range cfg.CredentialWaits {
		if key == "" {
			return fmt.Errorf("credential_waits requires an auth index or credential ID")
		}
		for _, value := range []*int{rule.MaxWaitSeconds, rule.FixedWaitSeconds} {
			if value != nil && (*value < 0 || *value > 86400) {
				return fmt.Errorf("credential wait settings must be null or 0..86400 seconds")
			}
		}
	}
	return nil
}

func (c *control) configure(raw []byte) error {
	var req struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}
	if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
		return errDecode
	}
	if req.SchemaVersion < 8 {
		return fmt.Errorf("session-quota-control requires host RPC schema 8; rebuild this repository's server")
	}
	cfg := defaultConfig()
	if errDecode := yaml.Unmarshal(req.ConfigYAML, &cfg); errDecode != nil {
		return fmt.Errorf("decode plugin configuration: %w", errDecode)
	}
	if errValidate := cfg.validate(); errValidate != nil {
		return errValidate
	}
	if errConfigure := c.evaluations.configure(cfg.EvaluationDBPath); errConfigure != nil {
		return fmt.Errorf("configure credential evaluations: %w", errConfigure)
	}
	if errConfigure := c.hub.configure(cfg.EvaluationDBPath + ".hub.json"); errConfigure != nil {
		return fmt.Errorf("configure Hub: %w", errConfigure)
	}
	c.report.configure(cfg.UsageEnabled, cfg.UsageDBPath)
	c.mu.Lock()
	c.cfg, c.closed = cfg, false
	c.mu.Unlock()
	return nil
}

func registration() any {
	defaults := defaultConfig()
	return struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  map[string]bool    `json:"capabilities"`
	}{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{Name: "session-quota-control", Version: "0.7.0", Author: "local",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "manual_assignment_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Default: defaults.ManualAssignment, Description: "Wait for manual credential assignment on a cold session binding."},
				{Name: "assignment_wait_seconds", Type: pluginapi.ConfigFieldTypeInteger, Default: defaults.AssignmentWait, Description: "Manual assignment deadline; 0 immediately uses native routing."},
				{Name: "quota_pacing_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Default: defaults.QuotaEnabled, Description: "Calculate one fixed delay after credential selection."},
				{Name: "safety_margin_percent", Type: pluginapi.ConfigFieldTypeNumber, Default: defaults.SafetyMargin, Description: "Deduct this percentage from the observed remaining five-hour quota."},
				{Name: "initial_request_percent", Type: pluginapi.ConfigFieldTypeNumber, Default: defaults.InitialCost, Description: "Per-request quota estimate for a model without measured samples; independent of tokens and effort."},
				{Name: "model_average_samples", Type: pluginapi.ConfigFieldTypeInteger, Default: defaults.ModelSamples, Description: "Number of recent attributable request costs averaged per provider/model across all credentials."},
				{Name: "concurrency_window_seconds", Type: pluginapi.ConfigFieldTypeInteger, Default: defaults.ConcurrencyWindow, Description: "Lookback for time-weighted average concurrency per credential, including sleeping requests."},
				{Name: "max_pending", Type: pluginapi.ConfigFieldTypeInteger, Default: defaults.MaxPending, Description: "Bound waiting assignments and sleeping requests."},
				{Name: "credential_waits", Type: pluginapi.ConfigFieldTypeObject, Default: map[string]any{}, Description: "Map auth index (preferred) or credential ID to max_wait_seconds and fixed_wait_seconds; null means unset, 0 means no wait. The cap applies to automatic and fixed delays."},
				{Name: "access_control", Type: pluginapi.ConfigFieldTypeObject, Default: defaults.AccessControl, Description: "Independent credential_rules (auth_index to API keys) and api_key_rules (API key to auth indexes). Each rule uses mode blacklist or whitelist and entries. Both sides must allow access; default is an empty blacklist."},
				{Name: "usage_report_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Default: defaults.UsageEnabled, Description: "Persist request history, token usage and manual/quota waiting times in SQLite."},
				{Name: "usage_db_path", Type: pluginapi.ConfigFieldTypeString, Default: defaults.UsageDBPath, Description: "SQLite database path for this plugin's usage report; ~/ is expanded."},
				{Name: "evaluation_db_path", Type: pluginapi.ConfigFieldTypeString, Default: defaults.EvaluationDBPath, Description: "SQLite database for credential evaluation history and scheduled tests; independent of usage reporting."},
			}},
		Capabilities: map[string]bool{"session_affinity_gate": true, "auth_candidate_filter": true, "request_interceptor": true, "request_lifecycle_plugin": true, "usage_plugin": true, "management_api": true},
	}
}
