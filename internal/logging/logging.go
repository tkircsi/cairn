// Package logging builds the loggers the rest of the program writes to.
//
// Construction only. Deciding *what* is worth recording belongs to the code that
// knows what happened, so nothing here knows about registries, blobs or HTTP.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Supported output formats.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// New builds a logger writing to w.
//
// The writer is a parameter rather than assumed to be stderr so that a test can
// point a logger at a buffer and assert on what was recorded.
func New(w io.Writer, format, level string) (*slog.Logger, error) {
	var parsed slog.Level

	// slog.Level already parses "debug", "info", "warn" and "error"
	// case-insensitively, along with offsets like "warn+2". Delegating means the
	// accepted spellings match what the level renders back as.
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}

	options := &slog.HandlerOptions{Level: parsed}

	var handler slog.Handler

	switch strings.ToLower(format) {
	case FormatText:
		handler = slog.NewTextHandler(w, options)
	case FormatJSON:
		handler = slog.NewJSONHandler(w, options)
	default:
		return nil, fmt.Errorf("invalid log format %q: want %q or %q",
			format, FormatText, FormatJSON)
	}

	return slog.New(handler), nil
}

// Discard returns a logger that drops every record.
//
// For tests: components here default to slog.Default, and a test that exercises
// a warning path would otherwise write it to the test's own output and look like
// a failure.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
