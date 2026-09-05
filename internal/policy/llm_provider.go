package policy

import (
	"context"
	"fmt"
	"strings"
)

// ProviderAnthropic and ProviderGoogle are the two supported LLM_PROVIDER
// values. Anthropic is the default when LLM_PROVIDER is unset, matching
// this project's original integration.
const (
	ProviderAnthropic = "anthropic"
	ProviderGoogle    = "google"
)

// NewLLMClient constructs the LLMClient for the configured provider. Only
// the API key matching the selected provider is required — an unrelated
// missing key (e.g. no GeminiAPIKey when provider is anthropic) is never an
// error, since ProposePolicy only ever calls whichever single client this
// returns.
func NewLLMClient(
	ctx context.Context,
	provider, anthropicAPIKey, geminiAPIKey string,
) (LLMClient, error) {
	switch normalizeProvider(provider) {
	case ProviderGoogle:
		if geminiAPIKey == "" {
			return nil, fmt.Errorf("llm: GEMINI_API_KEY is required when LLM_PROVIDER=google")
		}
		return NewGeminiClient(ctx, geminiAPIKey)
	case ProviderAnthropic:
		if anthropicAPIKey == "" {
			return nil, fmt.Errorf("llm: ANTHROPIC_API_KEY is required when LLM_PROVIDER=anthropic")
		}
		return NewAnthropicClient(anthropicAPIKey), nil
	default:
		return nil, fmt.Errorf(
			"llm: unknown LLM_PROVIDER %q (expected %q or %q)",
			provider, ProviderAnthropic, ProviderGoogle,
		)
	}
}

// normalizeProvider trims and lowercases provider, defaulting an unset
// value to ProviderAnthropic rather than treating "" as an unknown provider.
func normalizeProvider(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		return ProviderAnthropic
	}
	return p
}
