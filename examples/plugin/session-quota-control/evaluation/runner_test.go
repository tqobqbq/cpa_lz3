package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func responseFor(text string) json.RawMessage {
	body, _ := json.Marshal(map[string]any{
		"status": "completed",
		"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}},
		"usage":  map[string]any{"input_tokens": 12, "output_tokens": 5, "output_tokens_details": map[string]int{"reasoning_tokens": 2}},
	})
	return body
}

func testRunner(t *testing.T, answer func(map[string]any) (json.RawMessage, error)) *Runner {
	t.Helper()
	var next atomic.Int64
	var streams sync.Map
	return NewRunner(func(method string, request any) (json.RawMessage, error) {
		data := request.(map[string]any)
		switch method {
		case "host.model.execute_stream":
			body, errAnswer := answer(data)
			if errAnswer != nil {
				return nil, errAnswer
			}
			id := fmt.Sprint(next.Add(1))
			streams.Store(id, body)
			return json.Marshal(map[string]string{"stream_id": id})
		case "host.model.stream_read":
			body, ok := streams.Load(data["stream_id"])
			if !ok {
				return nil, context.Canceled
			}
			event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": body})
			payload := append([]byte("data: "), event...)
			return json.Marshal(map[string]any{"payload": payload, "done": true})
		case "host.model.stream_close":
			streams.Delete(data["stream_id"])
			return json.RawMessage(`{}`), nil
		default:
			t.Errorf("unexpected callback %s", method)
			return nil, fmt.Errorf("unexpected callback")
		}
	})
}

func TestRunnerPinsCredentialAndUsesSelectedEffort(t *testing.T) {
	for _, test := range []string{"candy", "fingerprint", "pelican"} {
		t.Run(test, func(t *testing.T) {
			var calls atomic.Int32
			runner := testRunner(t, func(request map[string]any) (json.RawMessage, error) {
				calls.Add(1)
				if request["auth_id"] != "selected" || request["forced_provider"] != "codex" ||
					request["model"] != DefaultModel || request["entry_protocol"] != "openai-response" {
					t.Errorf("credential-bound request = %#v", request)
				}
				var body map[string]any
				if errDecode := json.Unmarshal(request["body"].([]byte), &body); errDecode != nil {
					t.Fatal(errDecode)
				}
				if body["model"] != DefaultModel || body["reasoning"].(map[string]any)["effort"] != "low" {
					t.Errorf("request lost model or effort: %v", body["reasoning"])
				}
				// CPA receives the final request and owns translation and payload
				// rules; the plugin neither sends HTTP nor executes generated tools.
				input := body["input"].([]any)
				if input[0].(map[string]any)["type"] != "additional_tools" {
					t.Error("upstream Codex request context was not preserved")
				}
				if test == "fingerprint" {
					return responseFor(fingerprintAnswer), nil
				}
				if test == "pelican" {
					return responseFor("```html\n<html><svg></svg></html>\n```"), nil
				}
				return responseFor("答案是 21"), nil
			})
			options := Options{Model: DefaultModel, Effort: "low", Test: test}
			if test == "pelican" {
				options.Variant = "original"
			}
			result := runner.Run(context.Background(), Credential{"selected", "codex"}, options, nil)
			wantCalls := int32(1)
			if test == "fingerprint" {
				wantCalls = 3
				if result.Attribution == nil || result.Attribution.Used != 3 || result.Attribution.Probability == nil {
					t.Fatalf("missing calibrated fingerprint: %+v", result)
				}
			}
			if calls.Load() != wantCalls || result.Attempts != int(wantCalls) ||
				result.InputTokens != 12*int64(wantCalls) || result.OutputTokens != 5*int64(wantCalls) || result.Status == "failed" {
				t.Fatalf("calls=%d result=%+v", calls.Load(), result)
			}
			if test == "pelican" && !strings.Contains(result.HTML, "<svg>") {
				t.Fatal("pelican markup missing")
			}
		})
	}
}

func TestCandyWrongAnswerIsNotRetriedAndStandaloneRuleIsPreserved(t *testing.T) {
	for _, answer := range []string{"29", "121", "210", "21", "不是21"} {
		var calls int
		runner := testRunner(t, func(map[string]any) (json.RawMessage, error) {
			calls++
			return responseFor(answer), nil
		})
		result := runner.Run(context.Background(), Credential{"a", "codex"},
			Options{Model: "model", Effort: "none", Test: "candy"}, nil)
		expected := "wrong"
		if answer == "21" || answer == "不是21" {
			expected = "correct"
		}
		if calls != 1 || result.Status != expected {
			t.Fatalf("answer=%s calls=%d status=%s", answer, calls, result.Status)
		}
	}
}

type testStatusError int

func (e testStatusError) Error() string   { return "upstream rejected request" }
func (e testStatusError) StatusCode() int { return int(e) }

func TestRejectedCredentialsDoNotRetryAndToolCallsAreNeverExecuted(t *testing.T) {
	oldDelay := retryDelay
	retryDelay = 0
	t.Cleanup(func() { retryDelay = oldDelay })
	for _, status := range []int{401, 429, 503, 200} {
		calls := 0
		runner := testRunner(t, func(map[string]any) (json.RawMessage, error) {
			calls++
			if status != 200 {
				return nil, testStatusError(status)
			}
			return json.RawMessage(`{"status":"completed","output":[{"type":"custom_tool_call","name":"apply_patch"}]}`), nil
		})
		result := runner.Run(context.Background(), Credential{"a", "codex"},
			Options{Model: "model", Effort: "low", Test: "pelican", Variant: "original"}, nil)
		want := 1
		if status >= 500 || status == 200 {
			want = 3
		}
		if result.Status != "failed" || calls != want || result.HTML != "" {
			t.Fatalf("status=%d calls=%d result=%+v", status, calls, result)
		}
	}
}

func TestCancellationClosesModelStreamWithoutDeadline(t *testing.T) {
	reading, closed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	runner := NewRunner(func(method string, request any) (json.RawMessage, error) {
		switch method {
		case "host.model.execute_stream":
			return json.RawMessage(`{"stream_id":"stream"}`), nil
		case "host.model.stream_read":
			close(reading)
			<-closed
			return nil, context.Canceled
		case "host.model.stream_close":
			once.Do(func() { close(closed) })
			return json.RawMessage(`{}`), nil
		}
		return nil, fmt.Errorf("unexpected callback %s", method)
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	finished := make(chan Result, 1)
	go func() {
		finished <- runner.Run(ctx, Credential{"a", "codex"}, Options{Model: "model", Effort: "low", Test: "candy"}, nil)
	}()
	select {
	case <-reading:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not start")
	}
	cancel()
	select {
	case result := <-finished:
		if result.Status != "cancelled" || result.Attempts != 1 {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not close stream")
	}
}
