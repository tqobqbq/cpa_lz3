package evaluation

import "context"

// Probe supplies an external channel's transport while retaining the existing
// challenge selection, validation, retries, attribution and model matching.
type Probe func(context.Context, ProbeRequest) (ProbeResponse, error)

type ProbeRequest struct {
	Model, Effort, Instructions, Prompt string
}

type ProbeResponse struct {
	Text                                       string
	InputTokens, OutputTokens, ReasoningTokens int64
}

func NewProbeRunner(probe Probe) *Runner {
	return &Runner{probe: probe, slots: make(chan struct{}, fingerprintGlobalConcurrency)}
}

// RetryProbe marks a transient transport error for the existing bounded retry policy.
func RetryProbe(err error) error {
	if err == nil {
		return nil
	}
	return retryableError{err}
}
