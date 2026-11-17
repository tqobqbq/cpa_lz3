package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func secondsPointer(value int) *int { return &value }

func quotaRequest(id, index, model string) interceptCall {
	return interceptCall{RequestInterceptRequest: pluginapi.RequestInterceptRequest{
		RequestID: id, Model: model, ToFormat: "claude", Metadata: map[string]any{
			coreexecutor.SelectedAuthIndexMetadataKey: index, coreexecutor.SelectedAuthMetadataKey: "auth-" + index,
		},
	}}
}

func claudeSnapshot(index, used string, at, reset time.Time) pluginapi.HostAuthQuotaResponse {
	return pluginapi.HostAuthQuotaResponse{AuthID: "auth-" + index, Provider: "claude", ObservedAt: at, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": used, "Anthropic-Ratelimit-Unified-5h-Reset": reset.Format(time.RFC3339),
	}}
}

func TestQuotaDelayReadsOnceAndUsesOneUnchangedTimer(t *testing.T) {
	c, now := testControl()
	start, reset := *now, now.Add(2*time.Hour)
	queries, waits := 0, 0
	c.quota = func(index string) (pluginapi.HostAuthQuotaResponse, error) {
		queries++
		if queries > 2 {
			t.Fatal("quota was read beyond selection and the dashboard snapshot")
		}
		return claudeSnapshot(index, "0.5", *now, reset), nil
	}
	c.wait = func(_ string, delay time.Duration) error {
		waits++
		if delay != 160*time.Second {
			t.Fatalf("initial delay = %v, want 160s", delay)
		}
		c.mu.Lock()
		c.cfg.InitialCost, c.cfg.SafetyMargin = 50, 90
		c.cfg.CredentialWaits["index"] = credentialWait{FixedWaitSeconds: secondsPointer(1)}
		c.models[modelKey("claude", "opus")] = &modelAverage{samples: []float64{.9}}
		c.mu.Unlock()
		if queries != 1 {
			t.Fatal("pacing read quota more than once before starting its timer")
		}
		// The dashboard reads the host's cached snapshot independently. This
		// must not recalculate the delay already chosen by pacing.
		if response := c.state(); response.StatusCode != http.StatusOK {
			t.Fatal("dashboard failed")
		}
		*now = now.Add(delay)
		return nil
	}
	if response := c.pace(quotaRequest("request", "index", "opus")); response.Terminate {
		t.Fatalf("pacing failed: %+v", response)
	}
	if queries != 2 || waits != 1 || now.Sub(start) != 160*time.Second || c.waiting != 0 {
		t.Fatalf("queries=%d waits=%d elapsed=%v waiting=%d", queries, waits, now.Sub(start), c.waiting)
	}
	if !c.active["request"].readyAt.Equal(start.Add(160 * time.Second)) {
		t.Fatal("the chosen deadline changed")
	}
}

func TestFixedCredentialWaitAndCap(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fixed, cap *int
		want       time.Duration
	}{
		{"fixed", secondsPointer(30), nil, 30 * time.Second},
		{"fixed capped", secondsPointer(30), secondsPointer(7), 7 * time.Second},
		{"fixed zero", secondsPointer(0), nil, 0},
		{"cap zero", secondsPointer(30), secondsPointer(0), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, now := testControl()
			c.cfg.CredentialWaits["index"] = credentialWait{FixedWaitSeconds: tc.fixed, MaxWaitSeconds: tc.cap}
			c.quota = func(string) (pluginapi.HostAuthQuotaResponse, error) {
				t.Fatal("fixed override read quota")
				return pluginapi.HostAuthQuotaResponse{}, nil
			}
			waits := 0
			c.wait = func(_ string, delay time.Duration) error {
				waits++
				if delay != tc.want {
					t.Fatalf("delay=%v want=%v", delay, tc.want)
				}
				*now = now.Add(delay)
				return nil
			}
			if response := c.pace(quotaRequest("request", "index", "opus")); response.Terminate {
				t.Fatalf("response = %+v", response)
			}
			if (waits == 1) != (tc.want > 0) {
				t.Fatalf("timer calls = %d", waits)
			}
		})
	}
}

func TestAutomaticDelayCapAndExhaustedQuota(t *testing.T) {
	for _, tc := range []struct {
		used string
		cap  *int
		want time.Duration
	}{
		{"0.5", secondsPointer(10), 10 * time.Second},
		{"1", nil, 2 * time.Hour},
		{"1", secondsPointer(0), 0},
	} {
		c, now := testControl()
		c.cfg.CredentialWaits["index"] = credentialWait{MaxWaitSeconds: tc.cap}
		c.quota = func(index string) (pluginapi.HostAuthQuotaResponse, error) {
			return claudeSnapshot(index, tc.used, *now, now.Add(2*time.Hour)), nil
		}
		if response := c.pace(quotaRequest("request", "index", "opus")); response.Terminate {
			t.Fatalf("response = %+v", response)
		}
		if got := c.pools["index"].lastDelay; got != tc.want {
			t.Fatalf("used=%s delay=%v want=%v", tc.used, got, tc.want)
		}
	}
}

func TestConcurrencyUsesTimeWeightedHistory(t *testing.T) {
	_, now := testControl()
	start := *now
	var history concurrencyHistory
	history.change(start, 10)
	history.change(start.Add(150*time.Second), -8)
	end := start.Add(300 * time.Second)
	if got := history.average(end, 5*time.Minute); got != 6 {
		t.Fatalf("average concurrency=%v want=6", got)
	}
	history.change(end, 100)
	if got := history.average(end, 5*time.Minute); got != 6 {
		t.Fatalf("instantaneous spike replaced the historical estimate: %v", got)
	}
	if got := history.average(end.Add(5*time.Minute), 5*time.Minute); got != 102 {
		t.Fatalf("old history did not expire: %v", got)
	}
}

func recordMeasuredUsage(c *control, id, index, model, used string, at, reset time.Time) {
	snapshot := claudeSnapshot(index, used, at, reset)
	headers := make(http.Header)
	for name, value := range snapshot.Signals {
		headers.Set(name, value)
	}
	c.recordUsage(pluginapi.UsageRecord{RequestID: id, AuthID: snapshot.AuthID, AuthIndex: index,
		Provider: "claude", Model: model, RequestedAt: at, ResponseHeaders: headers})
}

func TestModelSlidingAverageIsSharedAcrossCredentialsAndIgnoresEffortTokens(t *testing.T) {
	c, now := testControl()
	c.cfg.ModelSamples, c.cfg.SafetyMargin = 2, 0
	start, reset := *now, now.Add(5*time.Hour)
	recordMeasuredUsage(c, "a0", "a", "opus(high)", "0.1", start, reset)
	recordMeasuredUsage(c, "a1", "a", "opus(low)", "0.12", start.Add(time.Second), reset)
	recordMeasuredUsage(c, "b0", "b", "opus", "0.3", start, reset)
	recordMeasuredUsage(c, "b1", "b", "opus", "0.34", start.Add(time.Second), reset)
	average := c.models[modelKey("claude", "opus")]
	if average == nil || math.Abs(average.mean(2)-.03) > 1e-9 {
		t.Fatalf("global model mean = %+v", average)
	}
	recordMeasuredUsage(c, "a2", "a", "opus", "0.18", start.Add(2*time.Second), reset)
	if got := average.mean(2); math.Abs(got-.05) > 1e-9 {
		t.Fatalf("sliding mean = %v, want last two costs (.04+.06)/2", got)
	}
	for index, body := range []string{`{"max_tokens":1,"reasoning":{"effort":"low"}}`, `{"max_tokens":999999,"reasoning":{"effort":"high"}}`} {
		c.quota = func(index string) (pluginapi.HostAuthQuotaResponse, error) {
			return claudeSnapshot(index, "0.5", *now, now.Add(1000*time.Second)), nil
		}
		req := quotaRequest("request", "new-credential", "opus(high)")
		req.Body = []byte(body)
		if response := c.pace(req); response.Terminate {
			t.Fatalf("response = %+v", response)
		}
		if got := c.pools["new-credential"].lastDelay; math.Abs(got.Seconds()-100) > .001 {
			t.Fatalf("body variant %d changed model cost: %v", index, got)
		}
		c.mu.Lock()
		c.finishLocked(req.RequestID, c.now())
		c.mu.Unlock()
	}
}

func TestRoundedQuotaCountsRequestsAndMixedModelIntervalsDoNotTrain(t *testing.T) {
	c, now := testControl()
	start, reset := *now, now.Add(5*time.Hour)
	recordMeasuredUsage(c, "baseline", "a", "opus", "0.1", start, reset)
	recordMeasuredUsage(c, "rounded-1", "a", "opus", "0.1", start.Add(time.Second), reset)
	recordMeasuredUsage(c, "rounded-2", "a", "opus", "0.1", start.Add(2*time.Second), reset)
	recordMeasuredUsage(c, "increase", "a", "opus", "0.13", start.Add(3*time.Second), reset)
	average := c.models[modelKey("claude", "opus")]
	if average == nil || len(average.samples) != 3 || math.Abs(average.mean(100)-.01) > 1e-9 {
		t.Fatalf("rounded request average=%+v", average)
	}
	recordMeasuredUsage(c, "mixed-1", "a", "sonnet", "0.13", start.Add(4*time.Second), reset)
	recordMeasuredUsage(c, "mixed-2", "a", "opus", "0.2", start.Add(5*time.Second), reset)
	if len(average.samples) != 3 || c.models[modelKey("claude", "sonnet")] != nil {
		t.Fatal("ambiguous mixed-model consumption was assigned to a model")
	}
}

func TestMissingQuotaDoesNotStartPollingOrWait(t *testing.T) {
	c, _ := testControl()
	queries := 0
	c.quota = func(string) (pluginapi.HostAuthQuotaResponse, error) {
		queries++
		return pluginapi.HostAuthQuotaResponse{Provider: "codex"}, nil
	}
	c.wait = func(string, time.Duration) error { t.Fatal("missing quota created a timer"); return nil }
	if response := c.pace(quotaRequest("request", "index", "gpt")); response.Terminate {
		t.Fatalf("response=%+v", response)
	}
	if queries != 1 {
		t.Fatalf("pacing polled quota: queries=%d", queries)
	}
	if response := c.state(); response.StatusCode != http.StatusOK || queries != 2 {
		t.Fatalf("dashboard must read one independent cached snapshot: queries=%d", queries)
	}
}

func TestCanceledFixedWaitCleansUpConcurrency(t *testing.T) {
	c, now := testControl()
	c.cfg.CredentialWaits["index"] = credentialWait{FixedWaitSeconds: secondsPointer(30)}
	c.wait = func(string, time.Duration) error { return context.Canceled }
	response := c.pace(quotaRequest("request", "index", "opus"))
	if response.StatusCode != 499 || !response.Terminate || c.waiting != 0 || len(c.active) != 0 || c.pools["index"].history.current != 0 {
		t.Fatalf("canceled wait leaked state: %+v", response)
	}
	c.finishLocked("request", *now)
	if c.waiting != 0 {
		t.Fatal("duplicate completion decremented waiting twice")
	}
}

func TestCredentialWaitSettingsPersistThroughConfiguration(t *testing.T) {
	c, _ := testControl()
	t.Cleanup(c.stop)
	databasePath, _ := json.Marshal(filepath.Join(t.TempDir(), "evaluations.sqlite"))
	raw, _ := json.Marshal(map[string]any{"schema_version": 8, "config_yaml": []byte("usage_report_enabled: false\nevaluation_db_path: " + string(databasePath) + "\ncredential_waits:\n  index:\n    max_wait_seconds: 15\n    fixed_wait_seconds: 0\n")})
	if errConfigure := c.configure(raw); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	rule := c.cfg.credentialWait("index", "auth")
	if rule.FixedWaitSeconds == nil || *rule.FixedWaitSeconds != 0 || rule.MaxWaitSeconds == nil || *rule.MaxWaitSeconds != 15 {
		t.Fatalf("settings=%+v", rule)
	}
	c.cfg.CredentialWaits["index"] = credentialWait{MaxWaitSeconds: secondsPointer(86401)}
	if errValidate := c.cfg.validate(); errValidate == nil {
		t.Fatal("unbounded manual wait accepted")
	}
}
