package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AnthropicClient is the real, production LLMClient implementation — a
// minimal, dependency-free HTTP client against Anthropic's Messages API.
// Kept intentionally small (no SDK) to match this project's existing
// preference for raw HTTP over heavy client libraries where a handful of
// fields is all that's needed (see internal/mandate's direct Razorpay
// calls).
type AnthropicClient struct {
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

// NewAnthropicClient constructs a client with sensible defaults. Model
// defaults to a fast, cheap model — this call is a structured single-turn
// extraction, not a task needing a large context window or deep reasoning.
func NewAnthropicClient(apiKey string) *AnthropicClient {
	return &AnthropicClient{
		APIKey:     apiKey,
		Model:      "claude-haiku-4-5-20251001",
		HTTPClient: &http.Client{Timeout: 20 * time.Second},
	}
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends prompt as a single user message and returns the model's
// text response verbatim — parseLLMResponse is responsible for extracting
// and validating JSON out of it, not this method.
func (c *AnthropicClient) Complete(ctx context.Context, prompt string) (string, error) {
	if c.APIKey == "" {
		return "", fmt.Errorf("anthropic: API key not set")
	}

	reqBody, err := json.Marshal(anthropicRequest{
		Model:     c.Model,
		MaxTokens: 1024,
		Messages:  []anthropicMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", fmt.Errorf("anthropic: failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody),
	)
	if err != nil {
		return "", fmt.Errorf("anthropic: failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("anthropic: failed to read response: %w", err)
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("anthropic: failed to unmarshal response: %w", err)
	}

	if parsed.Error != nil {
		return "", fmt.Errorf(
			"anthropic: API error (%s): %s",
			parsed.Error.Type,
			parsed.Error.Message,
		)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic: unexpected status %d: %s", resp.StatusCode, string(body))
	}
	if len(parsed.Content) == 0 {
		return "", fmt.Errorf("anthropic: response contained no content blocks")
	}

	return parsed.Content[0].Text, nil
}
