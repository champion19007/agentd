package model

import (
	"context"
	"sync"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// ScriptedModel is a deterministic fake implementing ports.Model for integration
// and adapter tests. It allows test code to simulate arbitrary model responses,
// schema variations, provider failures, latency, and context cancellation, while
// recording all inbound requests for assertion.
type ScriptedModel struct {
	mu        sync.Mutex
	calls     []ports.ModelRequest
	responses []ports.ModelResponse
	errs      []error
	handler   func(ctx context.Context, req ports.ModelRequest) (ports.ModelResponse, error)
	metrics   ports.Metrics
	modelName string
}

var _ ports.Model = (*ScriptedModel)(nil)

// NewScripted returns an empty ScriptedModel.
func NewScripted() *ScriptedModel {
	return &ScriptedModel{}
}

// WithMetrics configures the model to record token usage and costs to metrics.
func (m *ScriptedModel) WithMetrics(metrics ports.Metrics, modelName string) *ScriptedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.metrics = metrics
	m.modelName = modelName
	return m
}

// WithResponse queues a ModelResponse to be returned on subsequent Complete calls.
func (m *ScriptedModel) WithResponse(resp ports.ModelResponse) *ScriptedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses = append(m.responses, resp)
	m.errs = append(m.errs, nil)
	return m
}

// WithTextResponse is a convenience helper queuing a simple text response.
func (m *ScriptedModel) WithTextResponse(text string) *ScriptedModel {
	return m.WithResponse(ports.ModelResponse{
		Text:         text,
		InputTokens:  len(text) / 4,
		OutputTokens: len(text) / 4,
	})
}

// WithError queues an error to be returned on subsequent Complete calls.
func (m *ScriptedModel) WithError(err error) *ScriptedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses = append(m.responses, ports.ModelResponse{})
	m.errs = append(m.errs, err)
	return m
}

// WithHandler sets a custom callback to handle Complete calls dynamically.
func (m *ScriptedModel) WithHandler(h func(ctx context.Context, req ports.ModelRequest) (ports.ModelResponse, error)) *ScriptedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handler = h
	return m
}

// Complete implements ports.Model.
func (m *ScriptedModel) Complete(ctx context.Context, req ports.ModelRequest) (ports.ModelResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, req)
	handler := m.handler

	var resp ports.ModelResponse
	var err error
	if len(m.responses) > 0 {
		resp = m.responses[0]
		m.responses = m.responses[1:]
		err = m.errs[0]
		m.errs = m.errs[1:]
	}
	m.mu.Unlock()

	// Respect context cancellation / timeouts
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ports.ModelResponse{}, domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "model_timeout",
			Summary: "the model request timed out or was cancelled",
			Detail:  ctxErr.Error(),
		}
	}

	if handler != nil {
		resp, err = handler(ctx, req)
	}

	if err == nil && m.metrics != nil {
		mName := m.modelName
		if mName == "" {
			mName = "scripted-model"
		}
		purpose := string(req.Purpose)
		if purpose == "" {
			purpose = "unknown"
		}
		totalTokens := resp.InputTokens + resp.OutputTokens
		costMicros := int64(resp.InputTokens)*3 + int64(resp.OutputTokens)*15
		m.metrics.RecordModelUsage(mName, purpose, totalTokens, costMicros)
	}

	return resp, err
}

// Calls returns a copy of all requests recorded so far.
func (m *ScriptedModel) Calls() []ports.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make([]ports.ModelRequest, len(m.calls))
	copy(copied, m.calls)
	return copied
}

// LastCall returns the most recent request recorded, or panics if no calls occurred.
func (m *ScriptedModel) LastCall() ports.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		panic("ScriptedModel.LastCall called with 0 recorded calls")
	}
	return m.calls[len(m.calls)-1]
}

// CallCount returns the total number of calls received.
func (m *ScriptedModel) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// Reset clears all recorded calls and queued responses.
func (m *ScriptedModel) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
	m.responses = nil
	m.errs = nil
	m.handler = nil
}
