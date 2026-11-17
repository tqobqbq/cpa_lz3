package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/examples/plugin/session-quota-control/evaluation"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func newTestEvaluations(t *testing.T) (*evaluationManager, *atomic.Int64, chan evaluation.Options) {
	t.Helper()
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC).UnixNano())
	started := make(chan evaluation.Options, 20)
	s := newEvaluationManager(func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "a", Provider: "codex"}}, nil
	})
	s.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	s.run = func(ctx context.Context, auth evaluation.Credential, options evaluation.Options, progress func(evaluation.Result)) evaluation.Result {
		if auth.ID != "a" || auth.Provider != "codex" {
			t.Errorf("wrong credential: %+v", auth)
		}
		started <- options
		return evaluation.Result{Status: "completed", HTML: "<svg>saved evidence</svg>", Attempts: 1}
	}
	if errConfigure := s.configure(filepath.Join(t.TempDir(), "evaluations.sqlite")); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	t.Cleanup(s.stop)
	return s, &clock, started
}

func receiveEvaluation(t *testing.T, started <-chan evaluation.Options) evaluation.Options {
	t.Helper()
	select {
	case options := <-started:
		return options
	case <-time.After(5 * time.Second):
		t.Fatal("evaluation did not start")
		return evaluation.Options{}
	}
}

func TestDefaultEvaluationsScheduleExactlyFourJobs(t *testing.T) {
	s, clock, started := newTestEvaluations(t)
	plan := defaultEvaluationPlan()
	plan.CredentialID = "a"
	jobs, errAdd := s.add(plan, "codex")
	if errAdd != nil || len(jobs) != 4 {
		t.Fatalf("jobs=%+v error=%v", jobs, errAdd)
	}
	first, second := receiveEvaluation(t, started), receiveEvaluation(t, started)
	s.workers.Wait()
	if first.Test == second.Test || first.Model != "gpt-6-astra" || first.Effort != "low" ||
		second.Model != "gpt-6-astra" || second.Effort != "low" {
		t.Fatalf("default options = %+v, %+v", first, second)
	}
	records, _, errList := s.list("a")
	if errList != nil || len(records) != 4 {
		t.Fatalf("history = %+v, %v", records, errList)
	}
	pending := 0
	for _, job := range records {
		if job.Result.Status == "scheduled" {
			pending++
			if job.ScheduledAt.Sub(job.CreatedAt) != 5*time.Minute {
				t.Fatal("delay is not based on submission time")
			}
		}
		if job.Result.HTML != "" {
			t.Fatal("history list must not include large HTML payloads")
		}
	}
	if pending != 2 {
		t.Fatalf("pending = %d", pending)
	}
	clock.Add(int64(299 * time.Second))
	if delay := s.dispatch(); delay != time.Second {
		t.Fatalf("delay = %v", delay)
	}
	select {
	case <-started:
		t.Fatal("five-minute tests started early")
	default:
	}
	clock.Add(int64(time.Second))
	s.signal()
	receiveEvaluation(t, started)
	receiveEvaluation(t, started)
	s.workers.Wait()
	for _, job := range jobs {
		stored, errRead := s.record("a", job.ID)
		if errRead != nil || stored.Result.Status != "completed" || stored.Result.HTML != "<svg>saved evidence</svg>" {
			t.Fatalf("detail = %+v, %v", stored, errRead)
		}
		if _, errRead := s.record("other", job.ID); !errors.Is(errRead, sql.ErrNoRows) {
			t.Fatal("record was returned under the wrong credential")
		}
	}
}

func TestEvaluationRestartPreservesFutureJobsAndInterruptsActiveRecords(t *testing.T) {
	s, clock, started := newTestEvaluations(t)
	plan := defaultEvaluationPlan()
	plan.CredentialID, plan.Delays = "a", []int{300}
	jobs, errAdd := s.add(plan, "codex")
	if errAdd != nil {
		t.Fatal(errAdd)
	}
	s.mu.Lock()
	jobs[0].Result.Status = "running"
	errSave := s.saveLocked(jobs[0])
	path := s.path
	s.mu.Unlock()
	if errSave != nil {
		t.Fatal(errSave)
	}
	s.stop()
	if errConfigure := s.configure(path); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	interrupted, errRead := s.record("a", jobs[0].ID)
	if errRead != nil || interrupted.Result.Status != "interrupted" {
		t.Fatalf("interrupted record = %+v, %v", interrupted, errRead)
	}
	clock.Add(int64(5 * time.Minute))
	s.signal()
	options := receiveEvaluation(t, started)
	s.workers.Wait()
	if options.Test != jobs[1].Test {
		t.Fatal("restart replayed an interrupted job")
	}
	select {
	case <-started:
		t.Fatal("restart duplicated evaluation requests")
	default:
	}
}

func TestCancelScheduledAndRunningEvaluations(t *testing.T) {
	s, _, started := newTestEvaluations(t)
	plan := defaultEvaluationPlan()
	plan.CredentialID, plan.Tests, plan.Delays = "a", []string{"candy"}, []int{300}
	jobs, errAdd := s.add(plan, "codex")
	if errAdd != nil {
		t.Fatal(errAdd)
	}
	if errCancel := s.cancelJob("a", jobs[0].ID); errCancel != nil {
		t.Fatal(errCancel)
	}
	job, _ := s.record("a", jobs[0].ID)
	if job.Result.Status != "cancelled" {
		t.Fatal("scheduled job was not cancelled")
	}
	// No worker is active while replacing the runner.
	s.run = func(ctx context.Context, _ evaluation.Credential, options evaluation.Options, _ func(evaluation.Result)) evaluation.Result {
		started <- options
		<-ctx.Done()
		return evaluation.Result{Status: "cancelled"}
	}
	plan.Delays = []int{0}
	jobs, errAdd = s.add(plan, "codex")
	if errAdd != nil {
		t.Fatal(errAdd)
	}
	receiveEvaluation(t, started)
	if errCancel := s.cancelJob("a", jobs[0].ID); errCancel != nil {
		t.Fatal(errCancel)
	}
	s.workers.Wait()
	job, _ = s.record("a", jobs[0].ID)
	if job.Result.Status != "cancelled" || job.FinishedAt.IsZero() {
		t.Fatalf("running cancellation = %+v", job)
	}
}

func TestScheduledEvaluationRechecksCredential(t *testing.T) {
	s, clock, started := newTestEvaluations(t)
	var disabled atomic.Bool
	s.auths = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "a", Provider: "codex", Disabled: disabled.Load()}}, nil
	}
	plan := defaultEvaluationPlan()
	plan.CredentialID, plan.Tests, plan.Delays = "a", []string{"candy"}, []int{300}
	jobs, errAdd := s.add(plan, "codex")
	if errAdd != nil {
		t.Fatal(errAdd)
	}
	disabled.Store(true)
	clock.Add(int64(5 * time.Minute))
	s.dispatch()
	s.workers.Wait()
	job, _ := s.record("a", jobs[0].ID)
	if job.Result.Status != "skipped" {
		t.Fatalf("disabled credential = %+v", job)
	}
	select {
	case <-started:
		t.Fatal("disabled credential was executed")
	default:
	}
}

func TestEvaluationPlanRejectsInvalidCombinations(t *testing.T) {
	for _, body := range []string{
		`{"tests":[]}`, `{"delays":[]}`, `{"tests":["fingerprint","fingerprint"]}`,
		`{"delays":[0,0]}`, `{"delays":[-1]}`, `{"delays":[86401]}`,
		`{"model":"m(high)"}`, `{"model":""}`, `{"effort":"bad"}`,
		`{"tests":["unknown"]}`, `{"variant":"unknown"}`,
	} {
		plan := defaultEvaluationPlan()
		plan.CredentialID = "a"
		if errDecode := json.Unmarshal([]byte(body), &plan); errDecode != nil {
			t.Fatal(errDecode)
		}
		if _, errValidate := plan.options(); errValidate == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestCancelledHistoryIsBoundedWithoutDeletingPendingJobs(t *testing.T) {
	s, clock, _ := newTestEvaluations(t)
	plan := defaultEvaluationPlan()
	plan.CredentialID, plan.Tests, plan.Delays = "a", []string{"candy"}, []int{300}
	pending, errAdd := s.add(plan, "codex")
	if errAdd != nil {
		t.Fatal(errAdd)
	}
	for range evaluationHistoryLimit + 2 {
		clock.Add(1)
		jobs, errAdd := s.add(plan, "codex")
		if errAdd != nil {
			t.Fatal(errAdd)
		}
		if errCancel := s.cancelJob("a", jobs[0].ID); errCancel != nil {
			t.Fatal(errCancel)
		}
	}
	records, _, errList := s.list("a")
	if errList != nil || len(records) != evaluationHistoryLimit+1 {
		t.Fatalf("history count=%d, error=%v", len(records), errList)
	}
	job, errRead := s.record("a", pending[0].ID)
	if errRead != nil || job.Result.Status != "scheduled" {
		t.Fatal("pruning deleted an older pending job")
	}
}

func TestEvaluationManagementRoutesAndCredentialValidation(t *testing.T) {
	s, _, _ := newTestEvaluations(t)
	c := newController()
	c.evaluations = s
	c.auths = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "a", Provider: "codex"}, {ID: "off", Provider: "codex", Disabled: true}}, nil
	}
	c.extensionBase = "/v8/management/plugins/session-quota-control/extension"
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"credential_id":"a","delays":[300]}`, http.StatusCreated},
		{`{"credential_id":"off","delays":[300]}`, http.StatusConflict},
		{`{"credential_id":"missing","delays":[300]}`, http.StatusNotFound},
		{`{"credential_id":"a","tests":[]}`, http.StatusBadRequest},
	} {
		response := c.serve(pluginapi.ManagementRequest{Method: http.MethodPost, Path: c.extensionBase + "/evaluations", Body: []byte(tc.body)})
		if response.StatusCode != tc.want {
			t.Fatalf("request %s returned %d: %s", tc.body, response.StatusCode, response.Body)
		}
	}
	response := c.serve(pluginapi.ManagementRequest{Method: http.MethodGet, Path: c.extensionBase + "/evaluations", Query: url.Values{"credential_id": {"a"}}})
	if response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), `"scheduled"`) {
		t.Fatalf("history = %s", response.Body)
	}
	c.resourceBase = "/plugins/session-quota-control/resources"
	page := c.serve(pluginapi.ManagementRequest{Method: http.MethodGet, Path: c.resourceBase + "/dashboard"})
	if page.StatusCode != http.StatusOK || !strings.Contains(string(page.Body), `id="evaluationDialog"`) || strings.Contains(string(page.Body), "__EVALUATIONS_") {
		t.Fatal("evaluation UI was not embedded")
	}
	if strings.Contains(string(page.Body), "saved evidence") {
		t.Fatal("public page leaked private history")
	}
}
