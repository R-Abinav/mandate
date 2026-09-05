// Package config handles environment variable loading and validation.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

// Env holds the typed configuration loaded from environment variables.
type Env struct {
	RazorpayKeyID                 string
	RazorpayKeySecret             string
	AnthropicAPIKey               string
	AnthropicModel                string
	GeminiAPIKey                  string
	GeminiModel                   string
	LLMProvider                   string
	DatabaseURL                   string
	DatabaseURLTest               string
	DatabaseMaxOpenConnections    int
	DatabaseMaxIdleConnections    int
	DatabaseMaxConnectionLifetime time.Duration
}

// Load retrieves environment variables, optionally attempting to parse a .env
// file from the project root if it exists.
func Load() Env {
	// Walk up to find .env (useful since tests run in nested subdirectories)
	dir, _ := os.Getwd()
	for i := 0; i < 4; i++ {
		envPath := filepath.Join(dir, ".env")
		if err := godotenv.Load(envPath); err == nil {
			break
		}
		dir = filepath.Dir(dir)
	}

	parseInt := func(val string, fallback int) int {
		if val == "" {
			return fallback
		}
		parsed, err := strconv.Atoi(val)
		if err != nil {
			return fallback
		}
		return parsed
	}

	parseDuration := func(val string, fallback time.Duration) time.Duration {
		if val == "" {
			return fallback
		}
		parsed, err := time.ParseDuration(val)
		if err != nil {
			return fallback
		}
		return parsed
	}

	return Env{
		RazorpayKeyID:              os.Getenv("RAZORPAY_KEY_ID"),
		RazorpayKeySecret:          os.Getenv("RAZORPAY_KEY_SECRET"),
		AnthropicAPIKey:            os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:             os.Getenv("ANTHROPIC_MODEL"),
		GeminiAPIKey:               os.Getenv("GEMINI_API_KEY"),
		GeminiModel:                os.Getenv("GEMINI_MODEL"),
		LLMProvider:                os.Getenv("LLM_PROVIDER"),
		DatabaseURL:                os.Getenv("DATABASE_URL"),
		DatabaseURLTest:            os.Getenv("DATABASE_URL_TEST"),
		DatabaseMaxOpenConnections: parseInt(os.Getenv("DATABASE_MAX_OPEN_CONNECTIONS"), 25),
		DatabaseMaxIdleConnections: parseInt(os.Getenv("DATABASE_MAX_IDLE_CONNECTIONS"), 25),
		DatabaseMaxConnectionLifetime: parseDuration(
			os.Getenv("DATABASE_MAX_CONNECTION_LIFETIME"),
			5*time.Minute,
		),
	}
}
