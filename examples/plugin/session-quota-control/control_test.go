package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func testControl() (*control, *time.Time) {
	c := newController()
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.wait = func(_ string, delay time.Duration) error { now = now.Add(delay); return nil }
	c.auths = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "manual", AuthIndex: "index", Provider: "codex"}}, nil
	}
	return c, &now
}

func TestManualAssignmentAndTimeout(t *testing.T) {
	for _, mode := range []string{"assign", "timeout", "disabled", "canceled", "release"} {
		t.Run(mode, func(t *testing.T) {
			c, now := testControl()
			start := *now
			waits := 0
			c.wait = func(_ string, delay time.Duration) error {
				waits++
				switch mode {
				case "assign", "release":
					var id string
					for _, pending := range c.pending {
						id = pending.ID
					}
					body, _ := json.Marshal(map[string]any{"id": id, "auth_id": "manual", "release": mode == "release"})
					if resp := c.assign(body); resp.StatusCode != http.StatusOK {
						t.Fatalf("assignment: %s", resp.Body)
					}
				case "disabled":
					c.cfg.ManualAssignment = false
				case "canceled":
					return context.Canceled
				case "timeout":
					*now = now.Add(delay)
				}
				return nil
			}
			resp, errAwait := c.awaitAssignment(affinityCall{SessionAffinityRequest: pluginapi.SessionAffinityRequest{
				Provider: "codex", Model: "gpt", SessionID: "session", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "manual"}},
			}})
			if mode == "canceled" {
				if !errors.Is(errAwait, context.Canceled) {
					t.Fatalf("error = %v", errAwait)
				}
			} else if errAwait != nil {
				t.Fatal(errAwait)
			}
			wantID := ""
			if mode == "assign" {
				wantID = "manual"
			}
			if resp.AuthID != wantID || len(c.pending) != 0 || waits == 0 {
				t.Fatalf("response=%+v pending=%d waits=%d", resp, len(c.pending), waits)
			}
			if mode == "timeout" && now.Sub(start) != time.Minute {
				t.Fatalf("deadline elapsed = %v", now.Sub(start))
			}
		})
	}
}

func TestExpiredAndIneligibleAssignmentsAreRejected(t *testing.T) {
	c, now := testControl()
	c.pending["key"] = &pendingSession{ID: "pending", CreatedAt: *now, Candidates: []candidateView{{ID: "other"}}}
	body := []byte(`{"id":"pending","auth_id":"manual"}`)
	if resp := c.assign(body); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ineligible response = %d", resp.StatusCode)
	}
	*now = now.Add(time.Minute)
	if resp := c.assign(body); resp.StatusCode != http.StatusConflict {
		t.Fatalf("expired response = %d", resp.StatusCode)
	}
}

func TestAffinityDiagnosticsAppearInStateAndHistory(t *testing.T) {
	c, _ := testControl()
	diagnostics := map[string]any{"cache_key": "codex::conversation::gpt", "headers": map[string]any{"Session-Id": []string{"conversation"}}}
	c.wait = func(_ string, _ time.Duration) error {
		var state struct {
			Pending []pendingSession `json:"pending"`
		}
		if errDecode := json.Unmarshal(c.state().Body, &state); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(state.Pending) != 1 || state.Pending[0].Affinity["cache_key"] != diagnostics["cache_key"] {
			t.Fatalf("pending diagnostics = %+v", state.Pending)
		}
		body, _ := json.Marshal(map[string]string{"id": state.Pending[0].ID, "auth_id": "manual"})
		if response := c.assign(body); response.StatusCode != http.StatusOK {
			t.Fatalf("assignment: %s", response.Body)
		}
		return nil
	}
	response, errAwait := c.awaitAssignment(affinityCall{SessionAffinityRequest: pluginapi.SessionAffinityRequest{
		Provider: "codex", Model: "gpt", SessionID: "conversation", Diagnostics: diagnostics,
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "manual", Status: "active"}},
	}})
	if errAwait != nil || response.AuthID != "manual" || len(c.history) != 1 {
		t.Fatalf("response=%+v error=%v history=%+v", response, errAwait, c.history)
	}
	var state struct {
		History []affinityHistoryRecord `json:"affinity_history"`
	}
	if errDecode := json.Unmarshal(c.state().Body, &state); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(state.History) != 1 || state.History[0].Result != "manual_assignment" || state.History[0].ManualAuthID != "manual" || state.History[0].Session.Affinity["cache_key"] != diagnostics["cache_key"] {
		t.Fatalf("history diagnostics = %+v", state.History)
	}
}

func TestAffinityDiagnosticHistoryIsBounded(t *testing.T) {
	c, _ := testControl()
	c.wait = func(_ string, _ time.Duration) error {
		for _, pending := range c.pending {
			pending.released = true
		}
		return nil
	}
	for index := range 60 {
		_, errAwait := c.awaitAssignment(affinityCall{SessionAffinityRequest: pluginapi.SessionAffinityRequest{
			Provider: "codex", Model: "gpt", SessionID: strconv.Itoa(index),
		}})
		if errAwait != nil {
			t.Fatal(errAwait)
		}
	}
	if len(c.history) != 50 || c.history[0].Session.SessionID != "10" || c.history[49].Session.SessionID != "59" {
		t.Fatalf("retained history = %+v", c.history)
	}
}

func TestFiveHourWindowParsing(t *testing.T) {
	_, now := testControl()
	reset := now.Add(5 * time.Hour)
	codex := map[string]string{
		"X-Codex-Primary-Window-Minutes": "300", "X-Codex-Primary-Used-Percent": "42.5",
		"X-Codex-Primary-Reset-After-Seconds": "18000",
		"X-Codex-Secondary-Window-Minutes":    "10080", "X-Codex-Secondary-Used-Percent": "99",
		"X-Codex-Secondary-Reset-At": strconv.FormatInt(reset.Unix(), 10),
	}
	windows := readWindows("codex", codex, *now)
	if len(windows) != 1 || windows[0].Used != .425 || !windows[0].ResetAt.Equal(reset) {
		t.Fatalf("windows = %+v", windows)
	}
	claude := map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.25", "Anthropic-Ratelimit-Unified-5h-Reset": reset.Format(time.RFC3339)}
	if got := readWindows("claude", claude, *now); len(got) != 1 || got[0].Used != .25 {
		t.Fatalf("claude = %+v", got)
	}
	for _, invalid := range []string{"NaN", "Inf", "-0.5", ""} {
		claude["Anthropic-Ratelimit-Unified-5h-Utilization"] = invalid
		if got := readWindows("claude", claude, *now); len(got) != 0 {
			t.Fatalf("accepted %q: %+v", invalid, got)
		}
	}
	claude["Anthropic-Ratelimit-Unified-5h-Utilization"] = "1.1"
	if got := readWindows("claude", claude, *now); len(got) != 1 || got[0].Used != 1 {
		t.Fatal("over-limit usage was not treated as exhausted")
	}
}
