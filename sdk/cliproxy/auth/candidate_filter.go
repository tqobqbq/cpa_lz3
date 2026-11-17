package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type authCandidateFilterContextKey struct{}

func (m *Manager) authCandidateFilter() pluginapi.AuthCandidateFilter {
	m.mu.RLock()
	scheduler := m.pluginScheduler
	m.mu.RUnlock()
	filter, ok := scheduler.(pluginapi.AuthCandidateFilter)
	if !ok || filter == nil {
		return nil
	}
	if detector, ok := scheduler.(interface{ HasAuthCandidateFilters() bool }); ok && !detector.HasAuthCandidateFilters() {
		return nil
	}
	return filter
}

func credentialAccessTermination(status int, code, reason string) error {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{
		"type": "permission_error", "code": code, "message": reason,
	}})
	return &coreexecutor.RequestTerminatedError{HTTPStatus: status, Body: body,
		Header: http.Header{"Content-Type": {"application/json"}}}
}

func (m *Manager) filterAuthCandidates(ctx context.Context, providers []string, model string, opts coreexecutor.Options, candidates []*Auth) (map[string]string, error) {
	filter := m.authCandidateFilter()
	if filter == nil || len(candidates) == 0 {
		return nil, nil
	}
	response, errFilter := filter.FilterAuthCandidates(ctx, pluginapi.AuthCandidateFilterRequest{
		RequestID: coreexecutor.RequestLifecycleIDFromContext(ctx), APIKey: coreusage.APIKeyFromContext(ctx),
		Model: model, Providers: providers, Candidates: schedulerAuthCandidates(candidates),
	})
	if errContext := ctx.Err(); errContext != nil {
		return nil, errContext
	}
	if errFilter != nil {
		return nil, credentialAccessTermination(http.StatusServiceUnavailable, "credential_access_filter_failed", "Cannot evaluate credential access rules")
	}
	known := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		known[candidate.ID] = struct{}{}
	}
	for id := range response.Denied {
		if _, exists := known[id]; !exists {
			return nil, credentialAccessTermination(http.StatusServiceUnavailable, "credential_access_filter_failed", "Invalid credential access decision")
		}
	}
	if len(response.Denied) == len(candidates) {
		reason := "No credential is permitted for this API key"
		if detail := response.Denied[candidates[0].ID]; detail != "" {
			reason += ": " + detail
		}
		return nil, credentialAccessTermination(http.StatusForbidden, "credential_access_denied", reason)
	}
	return response.Denied, nil
}

// Candidate filtering precedes availability, priority and native affinity lookup.
// The same context constrains delegated built-in schedulers as well.
func (m *Manager) filterSelectionContext(ctx context.Context, providers []string, model string, opts coreexecutor.Options) (context.Context, error) {
	if m.authCandidateFilter() == nil {
		return ctx, nil
	}
	ctx = context.WithValue(ctx, authCandidateFilterContextKey{}, map[string]string(nil))
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerSet[canonicalSchedulingProvider(provider)] = struct{}{}
	}
	registryRef := registry.GetGlobalRegistry()
	m.mu.RLock()
	candidates := make([]*Auth, 0, len(m.auths))
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled || !eligibility.allows(candidate) || (pinned != "" && candidate.ID != pinned) {
			continue
		}
		provider := executorKeyFromAuth(candidate)
		if _, exists := providerSet[canonicalSchedulingProvider(provider)]; !exists {
			continue
		}
		if _, exists := m.executorLocked(provider); !exists || !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		candidates = append(candidates, candidate.Clone())
	}
	m.mu.RUnlock()
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	denied, errFilter := m.filterAuthCandidates(ctx, providers, model, opts, candidates)
	if errFilter != nil {
		return ctx, errFilter
	}
	return context.WithValue(ctx, authCandidateFilterContextKey{}, denied), nil
}

// Revalidate after interceptors, including cancellable manual/quota waits.
func (m *Manager) interceptAndValidateAuth(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req coreexecutor.Request, opts coreexecutor.Options, requestedModel string) (coreexecutor.Request, coreexecutor.Options, error) {
	req, opts, errIntercept := applyRequestAfterAuthInterceptor(ctx, executor, provider, req, opts, requestedModel)
	if errIntercept != nil {
		return req, opts, errIntercept
	}
	return req, opts, m.validateAuthAccess(ctx, auth, provider, requestedModel, opts)
}

func (m *Manager) validateAuthAccess(ctx context.Context, auth *Auth, provider, model string, opts coreexecutor.Options) error {
	_, errFilter := m.filterAuthCandidates(ctx, []string{provider}, model, opts, []*Auth{auth})
	return errFilter
}
