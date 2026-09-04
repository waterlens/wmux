package main

import (
	"log/slog"
	"regexp"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	t.Parallel()
	tests := map[string]slog.Level{
		"":        slog.LevelInfo,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for input, expected := range tests {
		input, expected := input, expected
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			actual, err := parseLogLevel(input)
			if err != nil || actual != expected {
				t.Fatalf("parseLogLevel(%q) = (%v, %v), want (%v, nil)", input, actual, err, expected)
			}
		})
	}
	if _, err := parseLogLevel("verbose"); err == nil {
		t.Fatal("invalid log level was accepted")
	}
}

func TestDataMuxNameIsStableAndScoped(t *testing.T) {
	t.Parallel()
	first := dataMuxName("/tmp/wmux-a")
	if first != dataMuxName("/tmp/wmux-a/../wmux-a") {
		t.Fatal("equivalent data paths produced different mux names")
	}
	if first == dataMuxName("/tmp/wmux-b") {
		t.Fatal("different data paths produced the same mux name")
	}
	if !regexp.MustCompile(`^wmux-[0-9a-f]{8}$`).MatchString(first) {
		t.Fatalf("unexpected mux name %q", first)
	}
}
