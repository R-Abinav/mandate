package policy

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/genai"
)

// GeminiClient is the Google Gemini LLMClient implementation, backed by the
// official google.golang.org/genai SDK against the Gemini API backend (not
// Vertex AI) — a raw-HTTP client would need to reimplement auth and request
// shaping the SDK already handles, unlike AnthropicClient's minimal surface.
type GeminiClient struct {
	client *genai.Client
	Model  string
}

// NewGeminiClient constructs a client with sensible defaults. Model defaults
// to a fast, cheap model — same rationale as NewAnthropicClient: this call
// is a structured single-turn extraction, not a task needing deep reasoning.
func NewGeminiClient(ctx context.Context, apiKey string) (*GeminiClient, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("gemini: API key not set")
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: failed to construct client: %w", err)
	}
	return &GeminiClient{client: client, Model: "gemini-2.5-flash"}, nil
}

// Complete sends prompt as a single-turn generation request and returns the
// model's text response verbatim — parseLLMResponse is responsible for
// extracting and validating JSON out of it, not this method.
func (c *GeminiClient) Complete(ctx context.Context, prompt string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	resp, err := c.client.Models.GenerateContent(callCtx, c.Model, genai.Text(prompt), nil)
	if err != nil {
		return "", fmt.Errorf("gemini: request failed: %w", err)
	}

	text := resp.Text()
	if text == "" {
		return "", fmt.Errorf("gemini: response contained no text")
	}
	return text, nil
}
