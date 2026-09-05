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

// LLMConfig groups the provider selection with both providers' credentials
// and model overrides — passed as one value rather than five positional
// strings, which would be easy to mix up (which string is a key vs a
// model). Only the fields matching the selected Provider are actually used;
// the other provider's fields being empty is never an error.
type LLMConfig struct {
	Provider        string
	AnthropicAPIKey string
	AnthropicModel  string // empty falls back to defaultAnthropicModel
	GeminiAPIKey    string
	GeminiModel     string // empty falls back to defaultGeminiModel
}

// NewLLMClient constructs the LLMClient for the configured provider. Only
// the API key matching the selected provider is required — an unrelated
// missing key (e.g. no GeminiAPIKey when provider is anthropic) is never an
// error, since ProposePolicy only ever calls whichever single client this
// returns.
func NewLLMClient(ctx context.Context, cfg LLMConfig) (LLMClient, error) {
	switch normalizeProvider(cfg.Provider) {
	case ProviderGoogle:
		if cfg.GeminiAPIKey == "" {
			return nil, fmt.Errorf("llm: GEMINI_API_KEY is required when LLM_PROVIDER=google")
		}
		return NewGeminiClient(ctx, cfg.GeminiAPIKey, cfg.GeminiModel)
	case ProviderAnthropic:
		if cfg.AnthropicAPIKey == "" {
			return nil, fmt.Errorf("llm: ANTHROPIC_API_KEY is required when LLM_PROVIDER=anthropic")
		}
		return NewAnthropicClient(cfg.AnthropicAPIKey, cfg.AnthropicModel), nil
	default:
		return nil, fmt.Errorf(
			"llm: unknown LLM_PROVIDER %q (expected %q or %q)",
			cfg.Provider, ProviderAnthropic, ProviderGoogle,
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
