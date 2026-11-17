package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type sessionAdmissionKey struct{}

type sessionAdmission struct {
	gate    pluginapi.SessionAffinityGate
	manager *Manager
	model   string
	retry   bool
}

func withSessionAdmission(ctx context.Context, scheduler PluginScheduler, manager *Manager, model string, retry bool) context.Context {
	gate, ok := scheduler.(pluginapi.SessionAffinityGate)
	if !ok || gate == nil {
		return ctx
	}
	if detector, okDetector := scheduler.(interface{ HasSessionAffinityGate() bool }); okDetector && !detector.HasSessionAffinityGate() {
		return ctx
	}
	return context.WithValue(ctx, sessionAdmissionKey{}, sessionAdmission{gate: gate, manager: manager, model: model, retry: retry})
}

func sessionAdmissionFromContext(ctx context.Context) (sessionAdmission, bool) {
	if ctx == nil {
		return sessionAdmission{}, false
	}
	admission, ok := ctx.Value(sessionAdmissionKey{}).(sessionAdmission)
	return admission, ok && admission.gate != nil
}

func refreshSessionCandidates(admission sessionAdmission, ctx context.Context, opts coreexecutor.Options, candidates []*Auth) ([]*Auth, error) {
	if admission.manager == nil {
		return candidates, nil
	}
	denied, errFilter := admission.manager.filterAuthCandidates(ctx, nil, admission.model, opts, candidates)
	if errFilter != nil {
		return nil, errFilter
	}
	refreshed := make([]*Auth, 0, len(candidates))
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	eligibility.deniedAuthIDs = denied
	for _, candidate := range candidates {
		current, exists := admission.manager.GetByID(candidate.ID)
		if !exists || current.Disabled || current.Status == StatusDisabled || !eligibility.allows(current) || canonicalSchedulingProvider(executorKeyFromAuth(current)) != canonicalSchedulingProvider(executorKeyFromAuth(candidate)) || !admission.manager.authSupportsRouteModel(registry.GetGlobalRegistry(), current, admission.model) {
			continue
		}
		blocked, _, _ := isAuthBlockedForModel(current, admission.manager.selectionModelForAuth(current, admission.model), time.Now())
		if !blocked {
			refreshed = append(refreshed, current)
		}
	}
	return refreshed, nil
}

func sessionManualChoiceKey(provider, model, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return provider + "::" + sessionID + "::" + canonicalModelKey(model)
}

func (s *SessionAffinitySelector) manualSessionAuthID(provider, model, sessionID string, opts coreexecutor.Options) string {
	authID, _ := s.manualChoices.Get(sessionManualChoiceKey(provider, model, sessionID))
	if authID == "" {
		return manualRequestAuthID(provider, model, sessionID, opts)
	}
	return authID
}

// Manual origin survives native error unbinding until a new decision or expiry.
func (s *SessionAffinitySelector) recordManualChoice(authID string, manual bool, keys ...string) {
	if manual {
		s.manualChoices.SetAliases(authID, keys...)
		return
	}
	if s.manualChoices == nil {
		return
	}
	s.manualChoices.mu.Lock()
	defer s.manualChoices.mu.Unlock()
	for _, key := range keys {
		if entry, exists := s.manualChoices.entries[key]; exists {
			s.manualChoices.removeAliasGroupLocked(entry)
		}
	}
}

const manualChoiceMetadataKey = "session_affinity_manual_choice"

type manualSessionChoice struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	SessionID string `json:"session_id"`
	AuthID    string `json:"auth_id"`
	Manual    bool   `json:"manual"`
}

func manualRequestAuthID(provider, model, sessionID string, opts coreexecutor.Options) string {
	choice, ok := opts.Metadata[manualChoiceMetadataKey].(manualSessionChoice)
	if !ok || !choice.Manual || choice.Provider != provider || choice.Model != canonicalModelKey(model) || choice.SessionID != sessionID {
		return ""
	}
	return choice.AuthID
}

// Request metadata retains origin even if quota pacing outlasts the session TTL.
func recordRequestManualChoice(provider, model, sessionID, authID string, manual bool, opts coreexecutor.Options) {
	if opts.Metadata == nil {
		return
	}
	opts.Metadata[manualChoiceMetadataKey] = manualSessionChoice{Provider: provider, Model: canonicalModelKey(model), SessionID: sessionID, AuthID: authID, Manual: manual}
}

// pickColdSession keeps manual choices inside the native affinity binding path.
func (s *SessionAffinitySelector) pickColdSession(ctx context.Context, provider, model, sessionID string, opts coreexecutor.Options, available []*Auth, previousManualAuthID string) (*Auth, bool, error) {
	fallback := func() (*Auth, bool, error) {
		auth, errPick := s.fallback.Pick(ctx, provider, model, opts, highestPriorityAuths(available))
		return auth, false, errPick
	}
	admission, enabled := sessionAdmissionFromContext(ctx)
	automaticRetry := admission.retry && previousManualAuthID == ""
	if choice, ok := opts.Metadata[manualChoiceMetadataKey].(manualSessionChoice); ok && admission.retry && choice.Provider == provider && choice.Model == canonicalModelKey(model) && !choice.Manual {
		automaticRetry = true
	}
	if !enabled || automaticRetry {
		return fallback()
	}
	diagnostics := s.sessionAdmissionDiagnostics(provider, model, sessionID, opts)
	diagnostics["credential_retry"] = admission.retry
	if previousManualAuthID != "" {
		diagnostics["pending_reason"] = "manual_credential_reassignment"
		diagnostics["previous_auth_id"] = previousManualAuthID
	}
	for {
		resp, errAwait := admission.gate.AwaitSessionAffinity(ctx, pluginapi.SessionAffinityRequest{
			APIKey:   coreusage.APIKeyFromContext(ctx),
			Provider: provider, Model: canonicalModelKey(model), SessionID: sessionID, Candidates: schedulerAuthCandidates(available),
			Diagnostics: diagnostics,
		})
		if errContext := ctx.Err(); errContext != nil {
			return nil, false, errContext
		}
		if errAwait != nil {
			return nil, false, errAwait
		}
		// Refresh the native eligible set without restoring excluded retry credentials.
		var errRefresh error
		available, errRefresh = refreshSessionCandidates(admission, ctx, opts, available)
		if errRefresh != nil {
			return nil, false, errRefresh
		}
		if resp.AuthID == "" {
			return fallback()
		}
		if auth := pickSchedulerAuthByID(available, resp.AuthID); auth != nil {
			return auth, true, nil
		}
		if len(available) == 0 {
			return fallback()
		}
		// A stale manual choice requires another decision instead of a silent switch.
		diagnostics["pending_reason"] = "manual_choice_unavailable"
		diagnostics["previous_auth_id"] = resp.AuthID
	}
}

type sessionAdmissionLock struct {
	refs int
	slot chan struct{}
}

type sessionAdmissionLocks struct {
	mu      sync.Mutex
	entries map[string]*sessionAdmissionLock
}

func (locks *sessionAdmissionLocks) acquire(ctx context.Context, key string) (func(), error) {
	locks.mu.Lock()
	if locks.entries == nil {
		locks.entries = make(map[string]*sessionAdmissionLock)
	}
	entry := locks.entries[key]
	if entry == nil {
		entry = &sessionAdmissionLock{slot: make(chan struct{}, 1)}
		locks.entries[key] = entry
	}
	entry.refs++
	locks.mu.Unlock()
	releaseRef := func() {
		locks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(locks.entries, key)
		}
		locks.mu.Unlock()
	}
	select {
	case entry.slot <- struct{}{}:
		return func() { <-entry.slot; releaseRef() }, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}

func pendingLCPSession(namespace string, fingerprints []string, prefixLength int) string {
	digest := sha256.Sum256([]byte(namespace + "\x00" + strings.Join(fingerprints[:prefixLength], "\x00")))
	return "lcp-pending:" + hex.EncodeToString(digest[:16])
}
