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

// defaultGeminiModel is used when model is empty. gemini-2.5-flash is
// deprecated; the Gemini API's own error response for it names
// gemini-3.6-flash as the replacement, used here rather than guessed at.
const defaultGeminiModel = "gemini-3.6-flash"

// NewGeminiClient constructs a client. model overrides the default
// (GEMINI_MODEL in .env, threaded through by NewLLMClient) — an empty
// string falls back to defaultGeminiModel, never an empty Model field
// silently sent to the API.
func NewGeminiClient(ctx context.Context, apiKey, model string) (*GeminiClient, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("gemini: API key not set")
	}
	if model == "" {
		model = defaultGeminiModel
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: failed to construct client: %w", err)
	}
	return &GeminiClient{client: client, Model: model}, nil
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
