package pluginapi

import "context"

// AuthCandidateFilter excludes credentials without replacing native scheduling.
// An error aborts selection; it must never fall back to unfiltered credentials.
type AuthCandidateFilter interface {
	FilterAuthCandidates(context.Context, AuthCandidateFilterRequest) (AuthCandidateFilterResponse, error)
}

type AuthCandidateFilterRequest struct {
	Plugin     Metadata                 `json:"plugin"`
	RequestID  string                   `json:"request_id"`
	APIKey     string                   `json:"api_key"`
	Model      string                   `json:"model"`
	Providers  []string                 `json:"providers"`
	Candidates []SchedulerAuthCandidate `json:"candidates"`
}

type AuthCandidateFilterResponse struct {
	// Denied maps candidate auth IDs to reasons. Omitted candidates remain eligible.
	Denied map[string]string `json:"denied,omitempty"`
}

// HostAPIKeysResponse lists the configured downstream keys for management UIs.
type HostAPIKeysResponse struct {
	APIKeys []string `json:"api_keys"`
}
