// Package evaluation runs the upstream candy, numeric fingerprint and pelican
// tests through CPA's credential-bound model callbacks. It never executes tools.
package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

const DefaultModel = "gpt-6-astra"
const DefaultEffort = "low"

type Credential struct {
	ID       string
	Provider string
}

type Options struct {
	Model   string `json:"model"`
	Effort  string `json:"effort"`
	Test    string `json:"test"`
	Variant string `json:"variant,omitempty"`
}

// Result retains the evidence as well as its verdict. A pelican's completed
// status means code was received, not that its visual quality was graded.
type Result struct {
	Status           string                  `json:"status"`
	Error            string                  `json:"error,omitempty"`
	Answer           string                  `json:"answer,omitempty"`
	HTML             string                  `json:"html,omitempty"`
	Attempts         int                     `json:"attempts"`
	DurationMS       int64                   `json:"duration_ms"`
	Done             int                     `json:"done"`
	Total            int                     `json:"total"`
	DetectorRevision string                  `json:"detector_revision,omitempty"`
	Samples          []fingerprintSample     `json:"samples,omitempty"`
	Attribution      *fingerprintAttribution `json:"attribution,omitempty"`
	ModelMatch       *bool                   `json:"model_match,omitempty"`
	usage
}

func (r Result) Summary() Result {
	r.Answer, r.HTML, r.Samples = "", "", nil
	if r.Attribution != nil {
		attribution := *r.Attribution
		attribution.Results, attribution.Diagnostics = nil, nil
		r.Attribution = &attribution
	}
	return r
}

func (o Options) Validate() error {
	if !validModelName(o.Model, false) || strings.TrimSpace(o.Model) != o.Model {
		return fmt.Errorf("请输入模型名称，推理强度请在 effort 中选择")
	}
	if !slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, o.Effort) {
		return fmt.Errorf("请选择有效的 effort")
	}
	switch o.Test {
	case "candy", "fingerprint":
		if o.Variant != "" {
			return fmt.Errorf("只有画鹈鹕需要选择版本")
		}
	case "pelican":
		if _, ok := findPelicanVariant(o.Variant); !ok {
			return fmt.Errorf("请选择画鹈鹕版本")
		}
	default:
		return fmt.Errorf("请选择检测项目")
	}
	return nil
}

func ModelNames() []string {
	names := []string{DefaultModel}
	for _, model := range sharedDetector.models {
		if model.ID != DefaultModel {
			names = append(names, model.ID)
		}
	}
	return names
}

type HostCall func(string, any) (json.RawMessage, error)

type Runner struct {
	call  HostCall
	probe Probe
	slots chan struct{}
}

func NewRunner(call HostCall) *Runner {
	return &Runner{call: call, slots: make(chan struct{}, fingerprintGlobalConcurrency)}
}

func (r *Runner) Run(ctx context.Context, auth Credential, options Options, progress func(Result)) (result Result) {
	started := time.Now()
	result = Result{Status: "running", Total: 1}
	defer func() {
		result.DurationMS = time.Since(started).Milliseconds()
		if recovered := recover(); recovered != nil {
			result.Status, result.Error = "failed", fmt.Sprintf("检测失败：%v", recovered)
		}
		if ctx.Err() != nil {
			result.Status, result.Error = "cancelled", "检测已取消"
		}
	}()
	if errValidate := options.Validate(); errValidate != nil {
		result.Status, result.Error = "failed", errValidate.Error()
		return
	}
	if options.Test == "fingerprint" {
		return r.runFingerprint(ctx, auth, options, progress)
	}
	prompt := candyPrompt
	var accept func(string) error
	if options.Test == "pelican" {
		variant, _ := findPelicanVariant(options.Variant)
		prompt = variant.Prompt
		accept = func(answer string) (err error) {
			result.HTML, err = variant.page(answer)
			return err
		}
	}
	output, attempts, errAsk := r.askCodex(ctx, auth, options.Model, options.Effort, "", prompt, accept)
	result.Attempts, result.usage, result.Done = attempts, output.usage, 1
	switch {
	case errAsk != nil:
		result.Status, result.Error = "failed", truncate(errAsk.Error(), 1000)
	case options.Test == "pelican":
		result.Status = "completed"
	case hasStandalone21(output.Answer):
		result.Status, result.Answer = "correct", truncate(output.Answer, 4000)
	default:
		result.Status, result.Answer = "wrong", truncate(output.Answer, 4000)
	}
	return
}

func (r *Runner) runFingerprint(ctx context.Context, auth Credential, options Options, progress func(Result)) Result {
	result := Result{Status: "running", Total: fingerprintTarget, DetectorRevision: detectorRevision}
	challenges := fingerprintChallenges()
	type event struct {
		index  int
		sample fingerprintSample
		output modelResponse
	}
	events := make(chan event, len(challenges))
	var workers sync.WaitGroup
	for i, challenge := range challenges {
		workers.Add(1)
		go func() {
			defer workers.Done()
			sample := fingerprintSample{fingerprintChallenge: challenge}
			output, attempts, errAsk := r.askCodex(ctx, auth, options.Model, options.Effort,
				challenge.Instructions, challenge.Prompt, func(answer string) (err error) {
					sample.Text, sample.Parsed, err = fingerprintText(answer, challenge.ExpectedCount)
					return err
				})
			sample.Attempts, sample.Valid = attempts, errAsk == nil
			if errAsk != nil {
				sample.Error = truncate(errAsk.Error(), 500)
			}
			events <- event{i, sample, output}
		}()
	}
	go func() { workers.Wait(); close(events) }()
	result.Samples = make([]fingerprintSample, len(challenges))
	for event := range events {
		result.Samples[event.index] = event.sample
		result.usage.add(event.output.usage)
		result.Attempts += event.sample.Attempts
		result.Done++
		if progress != nil {
			progress(result)
		}
	}
	outputs := make([]fingerprintOutput, len(challenges))
	for i, sample := range result.Samples {
		outputs[i] = fingerprintOutput{sample.Text, sample.ExpectedCount}
	}
	attribution, errAnalyze := analyzeFingerprint(outputs)
	if errAnalyze != nil {
		result.Status, result.Error = "failed", errAnalyze.Error()
		return result
	}
	result.Attribution, result.Status = &attribution, "completed"
	if attribution.Used < fingerprintTarget {
		result.Status = "partial"
	}
	// Unknown aliases cannot be classified as a mismatch solely because they are
	// absent from the bank. The underlying ranking and probabilities are unchanged.
	if attribution.Prediction != "" && slices.Contains(sharedDetector.ModelIDs, fingerprintModelID(options.Model)) {
		match := fingerprintModelID(options.Model) == fingerprintModelID(attribution.Prediction)
		result.ModelMatch = &match
	}
	return result
}

func mustDecode[T any](data []byte) T {
	var result T
	if errDecode := json.Unmarshal(data, &result); errDecode != nil {
		panic(errDecode)
	}
	return result
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
