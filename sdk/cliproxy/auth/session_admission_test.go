package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type admissionSchedulerStub struct {
	await func(context.Context, pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error)
}

func (stub admissionSchedulerStub) HasScheduler() bool { return false }
func (stub admissionSchedulerStub) PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	return pluginapi.SchedulerPickResponse{}, false, nil
}
func (stub admissionSchedulerStub) AwaitSessionAffinity(ctx context.Context, req pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error) {
	return stub.await(ctx, req)
}

func newAdmissionManager(t *testing.T, await func(context.Context, pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error)) (*Manager, string, string) {
	t.Helper()
	provider, model := "admission", "admission-model"
	manager := NewManager(nil, nil, nil)
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	t.Cleanup(selector.Stop)
	manager.SetSelector(selector)
	manager.SetPluginScheduler(admissionSchedulerStub{await: await})
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, auth := range []*Auth{
		{ID: "admission-high", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "10"}},
		{ID: "admission-low", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "0"}},
	} {
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	return manager, provider, model
}

func TestSessionAdmissionManualChoiceUsesNativeStickyCache(t *testing.T) {
	calls := 0
	manager, provider, model := newAdmissionManager(t, func(_ context.Context, req pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error) {
		calls++
		if req.SessionID == "" || len(req.Candidates) != 2 {
			t.Fatalf("admission request = %+v", req)
		}
		return pluginapi.SessionAffinityResponse{AuthID: "admission-low"}, nil
	})
	for range 2 {
		auth, errPick := manager.SelectAuth(context.Background(), provider, model, coreexecutor.Options{Headers: http.Header{"X-Session-Id": {"manual-session"}}})
		if errPick != nil || auth == nil || auth.ID != "admission-low" {
			t.Fatalf("auth=%+v error=%v", auth, errPick)
		}
	}
	if calls != 1 {
		t.Fatalf("gate calls=%d, want only the cold binding", calls)
	}
	if _, status := manager.LookupSessionAffinity(provider, model, "manual-session"); status != "bound" {
		t.Fatalf("native affinity observation = %s", status)
	}
}

func TestSessionAdmissionTimeoutDelegatesToNativePriority(t *testing.T) {
	manager, provider, model := newAdmissionManager(t, func(context.Context, pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error) {
		return pluginapi.SessionAffinityResponse{}, nil
	})
	auth, errPick := manager.SelectAuth(context.Background(), provider, model, coreexecutor.Options{Headers: http.Header{"Session-Id": {"timeout"}}})
	if errPick != nil || auth == nil || auth.ID != "admission-high" {
		t.Fatalf("auth=%+v error=%v", auth, errPick)
	}
}

func TestSessionAdmissionRevalidatesCredentialsAfterWait(t *testing.T) {
	var manager *Manager
	calls := 0
	manager, provider, model := newAdmissionManager(t, func(_ context.Context, req pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error) {
		calls++
		if calls > 1 {
			return pluginapi.SessionAffinityResponse{AuthID: req.Candidates[0].ID}, nil
		}
		auth, _ := manager.GetByID("admission-low")
		auth.Disabled = true
		if _, errUpdate := manager.Update(WithSkipPersist(context.Background()), auth); errUpdate != nil {
			t.Fatal(errUpdate)
		}
		return pluginapi.SessionAffinityResponse{AuthID: auth.ID}, nil
	})
	auth, errPick := manager.SelectAuth(context.Background(), provider, model, coreexecutor.Options{Headers: http.Header{"Session-Id": {"changed"}}})
	if errPick != nil || auth == nil || auth.ID != "admission-high" {
		t.Fatalf("auth=%+v error=%v", auth, errPick)
	}
}

func TestSessionAdmissionCancellationDoesNotBind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	manager, provider, model := newAdmissionManager(t, func(context.Context, pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error) {
		cancel()
		return pluginapi.SessionAffinityResponse{AuthID: "admission-low"}, nil
	})
	auth, errPick := manager.SelectAuth(ctx, provider, model, coreexecutor.Options{Headers: http.Header{"Session-Id": {"canceled"}}})
	if auth != nil || !errors.Is(errPick, context.Canceled) {
		t.Fatalf("auth=%+v error=%v", auth, errPick)
	}
	if _, status := manager.LookupSessionAffinity(provider, model, "canceled"); status != "unbound" {
		t.Fatalf("canceled session status = %s", status)
	}
}

func TestSessionAdmissionConcurrentColdRequestsShareBinding(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	manager, provider, model := newAdmissionManager(t, func(context.Context, pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return pluginapi.SessionAffinityResponse{AuthID: "admission-low"}, nil
	})
	var group sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		group.Go(func() {
			auth, errPick := manager.SelectAuth(context.Background(), provider, model, coreexecutor.Options{Headers: http.Header{"Session-Id": {"concurrent"}}})
			if errPick == nil && (auth == nil || auth.ID != "admission-low") {
				errPick = errors.New("concurrent request used a different binding")
			}
			results <- errPick
		})
	}
	<-entered
	close(release)
	group.Wait()
	close(results)
	for errPick := range results {
		if errPick != nil {
			t.Fatal(errPick)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("gate called %d times", calls.Load())
	}
}

func TestSessionAdmissionSupportsContentMatchedSessions(t *testing.T) {
	calls := 0
	manager, provider, model := newAdmissionManager(t, func(_ context.Context, req pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error) {
		calls++
		if !strings.HasPrefix(req.SessionID, "lcp-pending:") {
			t.Fatalf("content session identity = %s", req.SessionID)
		}
		if req.Diagnostics["mode"] != "content_lcp" || req.Diagnostics["content_matching_used"] != true {
			t.Fatalf("content diagnostics = %+v", req.Diagnostics)
		}
		metadata := req.Diagnostics["metadata"].(map[string]any)
		if len(metadata[coreexecutor.LCPFingerprintMetadataKey].([]string)) == 0 {
			t.Fatal("content diagnostics omitted fingerprints")
		}
		return pluginapi.SessionAffinityResponse{AuthID: "admission-low"}, nil
	})
	for range 2 {
		_, errPick := manager.SelectAuth(context.Background(), provider, model, coreexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAI,
			Metadata:        map[string]any{coreexecutor.CallerScopeMetadataKey: "admission-caller"},
			OriginalRequest: []byte(`{"messages":[{"role":"system","content":"You are a coding assistant."},{"role":"user","content":"Please explain how this application works."}]}`),
		})
		if errPick != nil {
			t.Fatal(errPick)
		}
	}
	if calls != 1 {
		t.Fatalf("content session gate calls = %d", calls)
	}
}

func TestSessionAdmissionCodexEffortVariantsUseNativeModelIdentity(t *testing.T) {
	var manager *Manager
	calls := 0
	manager, provider, model := newAdmissionManager(t, func(_ context.Context, req pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error) {
		calls++
		if req.Model != "admission-model" || req.Diagnostics["model_key"] != req.Model {
			t.Fatalf("gate did not use the native base model: %+v", req)
		}
		selector := manager.selector.(*SessionAffinitySelector)
		key := req.Provider + "\x00" + req.Model + "\x00" + req.SessionID
		selector.admissionLocks.mu.Lock()
		_, held := selector.admissionLocks.entries[key]
		selector.admissionLocks.mu.Unlock()
		if !held {
			t.Fatal("admission lock does not use the native cache model identity")
		}
		return pluginapi.SessionAffinityResponse{AuthID: "admission-low"}, nil
	})
	for _, effort := range []string{"high", "low", "medium"} {
		auth, errPick := manager.SelectAuth(context.Background(), provider, model+"("+effort+")", coreexecutor.Options{
			Headers:         http.Header{"Session-Id": {"codex-conversation"}},
			OriginalRequest: []byte(`{"prompt_cache_key":"cache-` + effort + `","input":"another turn"}`),
			Metadata:        map[string]any{coreexecutor.ExecutionSessionMetadataKey: "connection-" + effort},
		})
		if errPick != nil || auth == nil || auth.ID != "admission-low" {
			t.Fatalf("effort=%s auth=%+v error=%v", effort, auth, errPick)
		}
	}
	if calls != 1 {
		t.Fatalf("one Codex conversation required %d manual assignments", calls)
	}
}

func TestSessionAdmissionDiagnosticsShowIdentityInputsWithoutMutatingMetadata(t *testing.T) {
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	t.Cleanup(selector.Stop)
	metadata := map[string]any{
		coreexecutor.ExecutionSessionMetadataKey: "websocket-connection",
		coreexecutor.CallerScopeMetadataKey:      "caller-scope",
		"private_callback":                       func() {},
	}
	before := make(map[string]any)
	for key, value := range metadata {
		if key != "private_callback" {
			before[key] = value
		}
	}
	diagnostics := selector.sessionAdmissionDiagnostics("codex", "gpt-5.5(high)", "codex:conversation", coreexecutor.Options{
		Headers: http.Header{
			"Session-Id":            {"conversation"},
			"X-Codex-Turn-Metadata": {`{"session_id":"conversation","request_kind":"turn"}`},
			"Authorization":         {"private-key"},
			"Cookie":                {"private-cookie"},
		},
		OriginalRequest: []byte(`{"request":{"session_id":"body-session","prompt_cache_key":"body-cache","metadata":{"parent_session_id":"parent","user_id":"account-id","api_key":"private-body-key"},"conversation":{"id":"body-conversation"},"input":"private-message"}}`),
		Metadata:        metadata,
	})
	if diagnostics["mode"] != "client_identity" || diagnostics["cache_key"] != "codex::codex:conversation::gpt-5.5" || diagnostics["parent_cache_key"] != "codex::codex:parent::gpt-5.5" {
		t.Fatalf("unexpected lookup diagnostics: %+v", diagnostics)
	}
	fields := diagnostics["payload_fields"].(map[string]any)
	for key, want := range map[string]string{
		"request.session_id":                 "body-session",
		"request.prompt_cache_key":           "body-cache",
		"request.metadata.parent_session_id": "parent",
		"request.metadata.user_id":           "account-id",
		"request.conversation.id":            "body-conversation",
	} {
		if fields[key] != want {
			t.Errorf("payload field %s = %v, want %s", key, fields[key], want)
		}
	}
	headers := diagnostics["headers"].(map[string]any)
	if len(headers) != 2 || len(fields) != 5 || len(diagnostics["metadata"].(map[string]any)) != 2 {
		t.Fatalf("diagnostics included unrelated request data: %+v", diagnostics)
	}
	after := make(map[string]any)
	for key, value := range metadata {
		if key != "private_callback" {
			after[key] = value
		}
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("diagnostic collection changed routing metadata: before=%v after=%v", before, after)
	}
}

func TestSessionAdmissionCredentialRetryRespectsSelectionSource(t *testing.T) {
	for _, mode := range []string{"single", "mixed"} {
		for _, decision := range []string{"manual", "fallback"} {
			t.Run(mode+"/"+decision, func(t *testing.T) {
				calls := 0
				manager, provider, model := newAdmissionManager(t, func(_ context.Context, req pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error) {
					calls++
					if decision == "manual" {
						return pluginapi.SessionAffinityResponse{AuthID: req.Candidates[0].ID}, nil
					}
					return pluginapi.SessionAffinityResponse{}, nil
				})
				ctx := context.Background()
				opts := coreexecutor.Options{
					Headers:  http.Header{"Session-Id": {"codex-retry-session"}},
					Metadata: make(map[string]any),
				}
				pick := func(tried map[string]struct{}) (*Auth, error) {
					if mode == "mixed" {
						auth, _, _, errPick := manager.pickNextMixedLegacy(ctx, []string{provider}, model, opts, tried)
						return auth, errPick
					}
					auth, _, errPick := manager.pickNextLegacy(ctx, provider, model, opts, tried)
					return auth, errPick
				}
				first, errPick := pick(nil)
				if errPick != nil || first == nil || first.ID != "admission-high" || calls != 1 {
					t.Fatalf("first auth=%+v error=%v gate calls=%d", first, errPick, calls)
				}
				manager.MarkResult(ctx, Result{
					AuthID: first.ID, Provider: provider, Model: model, RouteModel: model,
					Error:   &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "mock upstream failure"},
					Options: opts, CredentialVersion: first.CredentialVersion, RegistrationEpoch: first.RegistrationEpoch,
				})
				if _, status := manager.LookupSessionAffinity(provider, model, "codex-retry-session"); status != "unbound" {
					t.Fatalf("failed execution did not clear the original binding: %s", status)
				}
				retry, errRetry := pick(map[string]struct{}{first.ID: {}})
				wantCalls := 1
				if decision == "manual" {
					wantCalls++
				}
				if errRetry != nil || retry == nil || retry.ID != "admission-low" || calls != wantCalls {
					t.Fatalf("retry auth=%+v error=%v gate calls=%d, want %d for %s selection", retry, errRetry, calls, wantCalls, decision)
				}
				bound, errBound := pick(nil)
				if errBound != nil || bound == nil || bound.ID != retry.ID || calls != wantCalls {
					t.Fatalf("rebound auth=%+v error=%v gate calls=%d", bound, errBound, calls)
				}
				opts.Headers.Set("Session-Id", "another-codex-session")
				if _, errNew := pick(nil); errNew != nil || calls != wantCalls+1 {
					t.Fatalf("new session error=%v gate calls=%d, want a new admission", errNew, calls)
				}
			})
		}
	}
}
