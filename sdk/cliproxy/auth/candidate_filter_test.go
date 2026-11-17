package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type candidateFilterScheduler struct {
	filter   func(pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error)
	await    func(pluginapi.SessionAffinityRequest) pluginapi.SessionAffinityResponse
	delegate bool
}

func (stub *candidateFilterScheduler) HasScheduler() bool           { return stub.delegate }
func (stub *candidateFilterScheduler) HasSessionAffinityGate() bool { return stub.await != nil }
func (stub *candidateFilterScheduler) PickAuth(_ context.Context, _ pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	return pluginapi.SchedulerPickResponse{Handled: stub.delegate, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin}, stub.delegate, nil
}
func (stub *candidateFilterScheduler) FilterAuthCandidates(_ context.Context, req pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
	return stub.filter(req)
}
func (stub *candidateFilterScheduler) AwaitSessionAffinity(_ context.Context, req pluginapi.SessionAffinityRequest) (pluginapi.SessionAffinityResponse, error) {
	return stub.await(req), nil
}

func expectCredentialTermination(t *testing.T, err error, status int, code string) {
	t.Helper()
	var terminated *coreexecutor.RequestTerminatedError
	if !errors.As(err, &terminated) || terminated.HTTPStatus != status || !strings.Contains(string(terminated.Body), code) {
		t.Fatalf("error=%v, want HTTP %d / %s", err, status, code)
	}
}

func TestCredentialFilterPrecedesPriorityAndBuiltinDelegation(t *testing.T) {
	for _, delegate := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "delegated"}[delegate], func(t *testing.T) {
			manager, provider, model := newAdmissionManager(t, nil)
			calls := 0
			manager.SetPluginScheduler(&candidateFilterScheduler{delegate: delegate, filter: func(req pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
				calls++
				if req.APIKey != "downstream-client" || len(req.Candidates) != 2 {
					t.Fatal("candidate filter lost the authenticated identity or lower priority candidates")
				}
				for _, candidate := range req.Candidates {
					if candidate.AuthIndex == "" {
						t.Fatal("candidate has no stable credential index")
					}
				}
				return pluginapi.AuthCandidateFilterResponse{Denied: map[string]string{"admission-high": "blocked"}}, nil
			}})
			ctx := coreusage.WithAPIKey(context.Background(), "downstream-client")
			auth, errPick := manager.SelectAuth(ctx, provider, model, coreexecutor.Options{Headers: http.Header{"Authorization": {"Bearer upstream-key"}}})
			if errPick != nil || auth == nil || auth.ID != "admission-low" || calls == 0 {
				t.Fatalf("selected=%+v error=%v filter_calls=%d", auth, errPick, calls)
			}
		})
	}
}

func TestCredentialFilterPinsAndFailuresNeverBypassRules(t *testing.T) {
	for _, test := range []struct {
		name        string
		denied      map[string]string
		filterError error
		pinned      string
		status      int
		code        string
	}{
		{name: "all denied", denied: map[string]string{"admission-high": "blocked", "admission-low": "blocked"}, status: 403, code: "credential_access_denied"},
		{name: "pinned denied", denied: map[string]string{"admission-high": "blocked"}, pinned: "admission-high", status: 403, code: "credential_access_denied"},
		{name: "filter error", filterError: errors.New("failed"), status: 503, code: "credential_access_filter_failed"},
		{name: "invalid decision", denied: map[string]string{"unknown": "blocked"}, status: 503, code: "credential_access_filter_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, provider, model := newAdmissionManager(t, nil)
			manager.SetPluginScheduler(&candidateFilterScheduler{filter: func(pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
				return pluginapi.AuthCandidateFilterResponse{Denied: test.denied}, test.filterError
			}})
			_, errPick := manager.SelectAuth(context.Background(), provider, model, coreexecutor.Options{Metadata: map[string]any{coreexecutor.PinnedAuthMetadataKey: test.pinned}})
			expectCredentialTermination(t, errPick, test.status, test.code)
		})
	}
}

func TestCredentialFilterRevalidatesExistingAffinity(t *testing.T) {
	manager, provider, model := newAdmissionManager(t, nil)
	denied := map[string]string{}
	manager.SetPluginScheduler(&candidateFilterScheduler{filter: func(pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
		return pluginapi.AuthCandidateFilterResponse{Denied: denied}, nil
	}})
	ctx := coreusage.WithAPIKey(context.Background(), "client")
	opts := coreexecutor.Options{Headers: http.Header{"X-Session-Id": {"sticky-access-session"}}}
	first, errPick := manager.SelectAuth(ctx, provider, model, opts)
	if errPick != nil || first.ID != "admission-high" {
		t.Fatalf("initial selection=%+v error=%v", first, errPick)
	}
	denied["admission-high"] = "access revoked"
	second, errPick := manager.SelectAuth(ctx, provider, model, opts)
	if errPick != nil || second == nil || second.ID != "admission-low" {
		t.Fatalf("revoked sticky credential reused: selected=%+v error=%v", second, errPick)
	}
}

func TestCredentialFilterRevalidatesManualChoiceAfterWait(t *testing.T) {
	manager, provider, model := newAdmissionManager(t, nil)
	denied := map[string]string{}
	waits := 0
	manager.SetPluginScheduler(&candidateFilterScheduler{filter: func(req pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
		response := pluginapi.AuthCandidateFilterResponse{Denied: map[string]string{}}
		for _, candidate := range req.Candidates {
			if reason := denied[candidate.ID]; reason != "" {
				response.Denied[candidate.ID] = reason
			}
		}
		return response, nil
	}, await: func(req pluginapi.SessionAffinityRequest) pluginapi.SessionAffinityResponse {
		waits++
		if req.APIKey != "client" {
			t.Fatal("manual admission lost the client identity")
		}
		if waits == 1 {
			denied["admission-high"] = "revoked during wait"
			return pluginapi.SessionAffinityResponse{AuthID: "admission-high"}
		}
		if len(req.Candidates) != 1 || req.Candidates[0].ID != "admission-low" {
			t.Fatal("revoked credential remained in manual candidates")
		}
		return pluginapi.SessionAffinityResponse{AuthID: "admission-low"}
	}})
	auth, errPick := manager.SelectAuth(coreusage.WithAPIKey(context.Background(), "client"), provider, model,
		coreexecutor.Options{Headers: http.Header{"X-Session-Id": {"manual-access-session"}}})
	if errPick != nil || auth == nil || auth.ID != "admission-low" || waits != 2 {
		t.Fatalf("manual selection=%+v error=%v waits=%d", auth, errPick, waits)
	}
}

type accessRecordingExecutor struct {
	schedulerTestExecutor
	calls     []string
	failFirst bool
}

func (e *accessRecordingExecutor) record(auth *Auth) error {
	e.calls = append(e.calls, auth.ID)
	if e.failFirst && len(e.calls) == 1 {
		return &Error{HTTPStatus: 500, Message: "retry another credential"}
	}
	return nil
}
func (e *accessRecordingExecutor) Execute(_ context.Context, auth *Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, e.record(auth)
}
func (e *accessRecordingExecutor) CountTokens(ctx context.Context, auth *Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}
func (e *accessRecordingExecutor) ExecuteStream(_ context.Context, auth *Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	if errRecord := e.record(auth); errRecord != nil {
		return nil, errRecord
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("data: done\n\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestCredentialFilterExecutionAndRetryPaths(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "websocket", "count"} {
		for _, revokedDuringWait := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/retry", true: "/revoked-after-wait"}[revokedDuringWait], func(t *testing.T) {
				manager, provider, model := newAdmissionManager(t, nil)
				executor := &accessRecordingExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: provider}, failFirst: !revokedDuringWait}
				manager.RegisterExecutor(executor)
				denied := map[string]string{"never-authorized": "blocked"}
				manager.SetPluginScheduler(&candidateFilterScheduler{filter: func(req pluginapi.AuthCandidateFilterRequest) (pluginapi.AuthCandidateFilterResponse, error) {
					response := pluginapi.AuthCandidateFilterResponse{Denied: map[string]string{}}
					for _, auth := range req.Candidates {
						if reason := denied[auth.ID]; reason != "" {
							response.Denied[auth.ID] = reason
						}
					}
					return response, nil
				}})
				_, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "never-authorized", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "100"}})
				if errRegister != nil {
					t.Fatal(errRegister)
				}
				registry.GetGlobalRegistry().RegisterClient("never-authorized", provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("never-authorized") })
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Metadata: map[string]any{}}
				if revokedDuringWait {
					opts.RequestAfterAuthInterceptor = func(context.Context, coreexecutor.RequestAfterAuthInterceptRequest) coreexecutor.RequestAfterAuthInterceptResponse {
						denied["admission-high"] = "revoked during quota wait"
						return coreexecutor.RequestAfterAuthInterceptResponse{}
					}
				}
				ctx := coreusage.WithAPIKey(context.Background(), "client")
				if mode == "websocket" {
					ctx = coreexecutor.WithDownstreamWebsocket(ctx)
				}
				req := coreexecutor.Request{Model: model, Payload: []byte(`{"messages":[]}`)}
				var err error
				switch mode {
				case "execute":
					_, err = manager.Execute(ctx, []string{provider}, req, opts)
				case "count":
					_, err = manager.ExecuteCount(ctx, []string{provider}, req, opts)
				case "stream", "websocket":
					opts.Stream = true
					var result *coreexecutor.StreamResult
					result, err = manager.ExecuteStream(ctx, []string{provider}, req, opts)
					if result != nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				}
				if revokedDuringWait {
					expectCredentialTermination(t, err, 403, "credential_access_denied")
					if len(executor.calls) != 0 {
						t.Fatal("upstream called after permission was revoked")
					}
				} else if err != nil || !reflect.DeepEqual(executor.calls, []string{"admission-high", "admission-low"}) {
					t.Fatalf("execution error=%v upstream_credentials=%v", err, executor.calls)
				}
			})
		}
	}
}
