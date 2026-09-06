package logging_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/tkircsi/cairn/internal/logging"
)

func TestNewJSON(t *testing.T) {
	var buf bytes.Buffer

	logger, err := logging.New(&buf, logging.FormatJSON, "info")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Info("blob committed", slog.String("digest", "sha256:abc"))

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, buf.String())
	}

	if record["msg"] != "blob committed" {
		t.Errorf("msg = %v, want %q", record["msg"], "blob committed")
	}

	if record["digest"] != "sha256:abc" {
		t.Errorf("digest = %v", record["digest"])
	}
}

func TestNewLevelFiltering(t *testing.T) {
	var buf bytes.Buffer

	logger, err := logging.New(&buf, logging.FormatText, "warn")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Info("should be dropped")
	logger.Warn("should be kept")

	out := buf.String()

	if strings.Contains(out, "should be dropped") {
		t.Errorf("info record survived a warn threshold: %q", out)
	}

	if !strings.Contains(out, "should be kept") {
		t.Errorf("warn record was dropped: %q", out)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name   string
		format string
		level  string
	}{
		{"unknown format", "yaml", "info"},
		{"unknown level", logging.FormatText, "chatty"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Reported rather than silently defaulted: a typo in a flag should not
			// quietly produce a logger that drops what the operator asked to see.
			if _, err := logging.New(&bytes.Buffer{}, tc.format, tc.level); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestNewAcceptsLevelSpellings(t *testing.T) {
	// slog parses these itself; the test pins that we delegate rather than
	// hand-rolling a switch that would accept fewer of them.
	for _, level := range []string{"debug", "INFO", "Warn", "error", "warn+2"} {
		if _, err := logging.New(&bytes.Buffer{}, logging.FormatText, level); err != nil {
			t.Errorf("level %q rejected: %v", level, err)
		}
	}
}

func TestDiscard(t *testing.T) {
	logger := logging.Discard()

	// Mostly a guard that Discard returns something usable rather than nil, since
	// callers store it in a struct field and log through it unconditionally.
	logger.Error("this must not panic and must not appear anywhere")

	if logger.Enabled(nil, slog.LevelError) { //nolint:staticcheck // nil context is the documented zero use
		t.Error("Discard should not be enabled at any level")
	}
}
