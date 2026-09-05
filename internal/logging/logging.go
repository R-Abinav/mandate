// Package logging is the single, project-wide construction point for the
// *slog.Logger every binary and internal/gateway thread through as an
// explicit dependency — never a package-level global, consistent with how
// audit.Store is already an optional injected field on PolicyRoundTripper
// rather than a global sink.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// ParseLevel maps a LOG_LEVEL string (debug/info/warn/error, case-insensitive,
// mirroring LLM_PROVIDER's own normalization style) to a slog.Level.
// Unset or unrecognized falls back to slog.LevelInfo — deliberately not
// slog.LevelDebug: Debug-level output includes per-attempt/per-retry
// tracing that would be a noisy surprise for anyone running this cold
// without having read .env.example first. A stranger who clones this repo
// gets sane output; LOG_LEVEL=debug is something you opt into.
func ParseLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// New constructs the single *slog.Logger a cmd/ binary passes down as an
// explicit dependency (to gateway.NewGatedClient, and to its own top-level
// error handling) — a text handler writing to os.Stderr, deliberately
// separate from os.Stdout, which cmd/mandate-cli and cmd/mandate-verify
// use for their own deliberate product output (a policy echo, a chain
// verification result) that must never be interleaved with or mistaken
// for operational log lines.
func New(levelRaw string) *slog.Logger {
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: ParseLevel(levelRaw),
	})
	return slog.New(handler)
}
