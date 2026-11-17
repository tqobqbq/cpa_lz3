package evaluation

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestExternalProbeUsesExistingFingerprintDetector(t *testing.T) {
	var calls atomic.Int32
	runner := NewProbeRunner(func(ctx context.Context, request ProbeRequest) (ProbeResponse, error) {
		calls.Add(1)
		if request.Model != DefaultModel || request.Effort != "high" || request.Prompt == "" {
			t.Errorf("external probe lost evaluation options: %+v", request)
		}
		if _, hasDeadline := ctx.Deadline(); hasDeadline {
			t.Error("probe was assigned a deadline")
		}
		return ProbeResponse{Text: fingerprintAnswer, InputTokens: 12, OutputTokens: 100}, nil
	})
	result := runner.Run(context.Background(), Credential{}, Options{Model: DefaultModel, Effort: "high", Test: "fingerprint"}, nil)
	if calls.Load() != 3 || result.Attribution == nil || result.Attribution.Used != 3 || result.DetectorRevision != detectorRevision || result.InputTokens != 36 || result.OutputTokens != 300 {
		t.Fatalf("external probe did not use the existing detector: %+v", result)
	}
	outputs := make([]fingerprintOutput, len(result.Samples))
	for i, sample := range result.Samples {
		outputs[i] = fingerprintOutput{sample.Text, sample.ExpectedCount}
	}
	want, errAnalyze := analyzeFingerprint(outputs)
	if errAnalyze != nil || want.Prediction != result.Attribution.Prediction || *want.Probability != *result.Attribution.Probability {
		t.Fatal("external and existing fingerprint results disagree")
	}
}
