package policy

import (
	"context"
	"testing"
)

func TestNewLLMClient_DefaultsToAnthropic(t *testing.T) {
	client, err := NewLLMClient(context.Background(), "", "anthropic-key", "")
	if err != nil {
		t.Fatalf("expected no error for empty provider, got: %v", err)
	}
	if _, ok := client.(*AnthropicClient); !ok {
		t.Fatalf("expected *AnthropicClient, got %T", client)
	}
}

func TestNewLLMClient_ExplicitAnthropic(t *testing.T) {
	client, err := NewLLMClient(context.Background(), "Anthropic", "anthropic-key", "")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if _, ok := client.(*AnthropicClient); !ok {
		t.Fatalf("expected *AnthropicClient, got %T", client)
	}
}

func TestNewLLMClient_Google(t *testing.T) {
	client, err := NewLLMClient(context.Background(), "google", "", "gemini-key")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if _, ok := client.(*GeminiClient); !ok {
		t.Fatalf("expected *GeminiClient, got %T", client)
	}
}

func TestNewLLMClient_GoogleCaseInsensitiveWithWhitespace(t *testing.T) {
	client, err := NewLLMClient(context.Background(), "  GOOGLE  ", "", "gemini-key")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if _, ok := client.(*GeminiClient); !ok {
		t.Fatalf("expected *GeminiClient, got %T", client)
	}
}

// TestNewLLMClient_MissingAnthropicKey confirms selecting anthropic without
// ANTHROPIC_API_KEY fails clearly rather than constructing a client that
// would only fail later, mid-request, against the real API.
func TestNewLLMClient_MissingAnthropicKey(t *testing.T) {
	_, err := NewLLMClient(context.Background(), "anthropic", "", "gemini-key")
	if err == nil {
		t.Fatal("expected an error when ANTHROPIC_API_KEY is missing, got success")
	}
}

// TestNewLLMClient_MissingGeminiKey mirrors the above for google — and
// confirms a present ANTHROPIC_API_KEY does not paper over a missing
// GEMINI_API_KEY when google is the selected provider.
func TestNewLLMClient_MissingGeminiKey(t *testing.T) {
	_, err := NewLLMClient(context.Background(), "google", "anthropic-key", "")
	if err == nil {
		t.Fatal("expected an error when GEMINI_API_KEY is missing, got success")
	}
}

func TestNewLLMClient_UnknownProvider(t *testing.T) {
	_, err := NewLLMClient(context.Background(), "openai", "anthropic-key", "gemini-key")
	if err == nil {
		t.Fatal("expected an error for an unsupported provider, got success")
	}
}
