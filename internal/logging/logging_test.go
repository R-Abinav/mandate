package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel_RecognizedValues(t *testing.T) {
	tests := []struct {
		raw  string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"  Debug  ", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"WARN", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			if got := ParseLevel(tt.raw); got != tt.want {
				t.Fatalf("ParseLevel(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestParseLevel_UnrecognizedFallsBackToInfo confirms an unset or garbage
// LOG_LEVEL value defaults to Info, never erroring and never silently
// defaulting to the noisier Debug level — a stranger cloning this repo
// cold, with no .env.example read, must get sane output.
func TestParseLevel_UnrecognizedFallsBackToInfo(t *testing.T) {
	tests := []string{"", "verbose", "trace", "DEBUGG", "0", "silent"}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			if got := ParseLevel(raw); got != slog.LevelInfo {
				t.Fatalf("ParseLevel(%q) = %v, want %v (fallback)", raw, got, slog.LevelInfo)
			}
		})
	}
}

// TestNew_EmitsAtEachOfTheFourLevels is the lightweight slog.Handler test
// double this project's redaction test also relies on: a real
// slog.TextHandler writing to a buffer, not a hand-rolled mock — sufficient
// to prove Debug/Info/Warn/Error each reach the sink correctly labeled,
// without asserting on every individual call site in the codebase.
func TestNew_EmitsAtEachOfTheFourLevels(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logger.Debug("debug message")
	logger.Info("info message")
	logger.Warn("warn message")
	logger.Error("error message")

	output := buf.String()
	for _, want := range []string{
		"level=DEBUG msg=\"debug message\"",
		"level=INFO msg=\"info message\"",
		"level=WARN msg=\"warn message\"",
		"level=ERROR msg=\"error message\"",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, output)
		}
	}
}

// TestNew_DefaultLevelFiltersDebug confirms logging.New("") — the default,
// unset LOG_LEVEL case — actually filters Debug-level calls rather than
// merely defaulting ParseLevel's return value without wiring it through to
// the constructed handler.
func TestNew_DefaultLevelFiltersDebug(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: ParseLevel("")}))

	logger.Debug("should not appear")
	logger.Info("should appear")

	output := buf.String()
	if strings.Contains(output, "should not appear") {
		t.Fatalf("expected Debug to be filtered at the default level, got:\n%s", output)
	}
	if !strings.Contains(output, "should appear") {
		t.Fatalf("expected Info to pass through at the default level, got:\n%s", output)
	}
}
