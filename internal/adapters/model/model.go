// Package model is a driven adapter implementing ports.Model against BYOK
// providers.
//
// BYOK is a constraint on this file more than anywhere else. The key belongs to
// the operator, is resolved through a SecretResolver at call time, and is never
// stored, logged or echoed back. The provider is configuration: Agentd has no
// opinion about which model an operator pays for, and the core has no idea one
// exists.
//
// The wire format here is the Anthropic Messages API, which several providers
// also speak. A provider that speaks something else needs a new implementation
// of ports.Model and nothing else -- that is what the port is for.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// Defaults for an unconfigured adapter.
const (
	DefaultEndpoint  = "https://api.anthropic.com/v1/messages"
	DefaultModel     = "claude-opus-5"
	DefaultVersion   = "2023-06-01"
	DefaultTimeout   = 90 * time.Second
	DefaultMaxTokens = 2048
)

// Options configure a Client.
type Options struct {
	// Endpoint is the completions URL. Empty means DefaultEndpoint.
	Endpoint string

	// Model names the model to use. Empty means DefaultModel.
	Model string

	// APIKeyRef names the operator's key. It is resolved per call, so a key
	// rotated on disk takes effect without restarting Agentd.
	APIKeyRef domain.SecretRef

	// Secrets resolves APIKeyRef.
	Secrets ports.SecretResolver

	// MaxTokens caps a response when the request does not. Zero means
	// DefaultMaxTokens.
	MaxTokens int

	// Timeout bounds one call. Zero means DefaultTimeout.
	Timeout time.Duration

	// Client overrides the HTTP client, for tests.
	Client *http.Client

	// AnthropicVersion sets the api-version header. Empty means
	// DefaultVersion.
	AnthropicVersion string
}

// Client calls a BYOK provider.
type Client struct {
	opts Options
	http *http.Client
}

var _ ports.Model = (*Client)(nil)

// New builds a Client.
func New(opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	c := opts.Client
	if c == nil {
		c = &http.Client{Timeout: timeout}
	}
	if opts.Endpoint == "" {
		opts.Endpoint = DefaultEndpoint
	}
	if opts.Model == "" {
		opts.Model = DefaultModel
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = DefaultMaxTokens
	}
	if opts.AnthropicVersion == "" {
		opts.AnthropicVersion = DefaultVersion
	}
	return &Client{opts: opts, http: c}
}

// wire types, kept unexported so the provider's shape never escapes.

type wireRequest struct {
	Model       string        `json:"model"`
	MaxTokens   int           `json:"max_tokens"`
	System      string        `json:"system,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	Messages    []wireMessage `json:"messages"`
}

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type wireResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete asks the provider for a completion.
func (c *Client) Complete(ctx context.Context, req ports.ModelRequest) (ports.ModelResponse, error) {
	key, err := c.key(ctx)
	if err != nil {
		return ports.ModelResponse{}, err
	}

	maxTokens := req.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = c.opts.MaxTokens
	}

	body := wireRequest{
		Model:     c.opts.Model,
		MaxTokens: maxTokens,
		System:    req.System,
		Messages:  []wireMessage{{Role: "user", Content: req.Prompt}},
	}
	if req.Deterministic {
		// The same break should produce the same proposal, so that an operator
		// reviewing one twice sees the same thing.
		zero := 0.0
		body.Temperature = &zero
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return ports.ModelResponse{}, fmt.Errorf("model: encoding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return ports.ModelResponse{}, fmt.Errorf("model: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", c.opts.AnthropicVersion)
	httpReq.Header.Set("x-api-key", key.Reveal())

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return ports.ModelResponse{}, domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "model_unreachable",
			Summary: "the model provider could not be reached",
			Detail:  err.Error(),
		}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return ports.ModelResponse{}, domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "model_read_failed",
			Summary: "the connection to the model provider dropped",
			Detail:  err.Error(),
		}
	}

	if f := classify(resp.StatusCode, raw); f != nil {
		return ports.ModelResponse{}, *f
	}

	var decoded wireResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ports.ModelResponse{}, domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "model_unreadable",
			Summary: "the model provider's answer could not be read",
			Detail:  err.Error(),
		}
	}

	var text strings.Builder
	for _, block := range decoded.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}

	return ports.ModelResponse{
		Text:         text.String(),
		InputTokens:  decoded.Usage.InputTokens,
		OutputTokens: decoded.Usage.OutputTokens,
		// A truncated repair proposal is discarded rather than parsed
		// optimistically, so reporting this accurately matters.
		Truncated: decoded.StopReason == "max_tokens",
	}, nil
}

// key resolves the operator's credential for this call.
func (c *Client) key(ctx context.Context) (domain.Secret, error) {
	if c.opts.Secrets == nil || c.opts.APIKeyRef == "" {
		return domain.Secret{}, domain.Failure{
			Class:   domain.ClassAuth,
			Code:    "model_key_unset",
			Summary: "no model provider key is configured, so Agentd cannot work out a repair",
		}
	}
	bundle, err := c.opts.Secrets.Resolve(ctx, []domain.SecretRef{c.opts.APIKeyRef})
	if err != nil {
		return domain.Secret{}, err
	}
	key, ok := bundle.Get(c.opts.APIKeyRef)
	if !ok {
		return domain.Secret{}, domain.Failure{
			Class:   domain.ClassAuth,
			Code:    "model_key_missing",
			Summary: fmt.Sprintf("the model provider key %q is not configured", c.opts.APIKeyRef),
		}
	}
	return key, nil
}

// classify turns a provider status into a failure.
//
// The provider's own error text goes in Detail, never in Summary: a message
// written for an API consumer is rarely one an operator wants to read, and it
// can contain fragments of the request.
func classify(status int, body []byte) *domain.Failure {
	if status >= 200 && status < 300 {
		return nil
	}

	detail := string(body)
	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded); err == nil && decoded.Error != nil {
		detail = decoded.Error.Type + ": " + decoded.Error.Message
	}

	switch {
	case status == http.StatusTooManyRequests:
		return &domain.Failure{
			Class:   domain.ClassRateLimited,
			Code:    "model_rate_limited",
			Summary: "the model provider is rate limiting Agentd",
			Detail:  detail,
		}
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return &domain.Failure{
			Class:   domain.ClassAuth,
			Code:    "model_unauthorised",
			Summary: "the model provider rejected the configured key",
			Detail:  detail,
		}
	case status == http.StatusPaymentRequired:
		return &domain.Failure{
			Class:   domain.ClassAuth,
			Code:    "model_billing",
			Summary: "the model provider reports a billing problem with this account",
			Detail:  detail,
		}
	case status >= 500:
		return &domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "model_server_error",
			Summary: "the model provider is having trouble of its own",
			Detail:  detail,
		}
	default:
		return &domain.Failure{
			Class:   domain.ClassTransient,
			Code:    fmt.Sprintf("model_http_%d", status),
			Summary: "the model provider refused the request",
			Detail:  detail,
		}
	}
}
