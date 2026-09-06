package ocihttp

import (
	"io"
	"log/slog"
	"net/http"
	"time"
)

// maxLoggedPathLength bounds the request path in a log record.
//
// The path is attacker-controlled and unbounded in length, so logging it whole
// lets a client choose how much disk each request costs.
const maxLoggedPathLength = 256

// WithLogging wraps h in an access log.
//
// Structured rather than formatted, and that is a correctness property, not a
// stylistic one: the path and repository name come from the client, and slog
// escapes them as values. A printf-style line would let a request carrying CRLF
// forge a second log entry.
func WithLogging(h http.Handler, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Default to 200 because a handler that writes a body without calling
		// WriteHeader has implicitly sent one.
		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

		h.ServeHTTP(recorder, r)

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", truncate(r.URL.Path, maxLoggedPathLength)),
			slog.Int("status", recorder.status),
			slog.Int64("bytes", recorder.written),
			slog.Duration("duration", time.Since(start)),
		}

		// The session id is the natural correlation key for an upload: one blob
		// arrives as a POST, some number of PATCHes and a PUT, and this is the only
		// field common to all of them.
		if id := recorder.Header().Get("Docker-Upload-UUID"); id != "" {
			attrs = append(attrs, slog.String("upload", id))
		}

		if dgst := recorder.Header().Get("Docker-Content-Digest"); dgst != "" {
			attrs = append(attrs, slog.String("digest", dgst))
		}

		logger.LogAttrs(r.Context(), levelFor(recorder.status), "request", attrs...)
	})
}

// levelFor keeps the log readable at a glance: a client's mistake is not the
// same event as the registry failing.
func levelFor(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	return s[:limit] + "...(truncated)"
}

// responseRecorder observes the status and byte count of a response.
type responseRecorder struct {
	http.ResponseWriter

	status      int
	written     int64
	wroteHeader bool
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}

	w.status = status
	w.wroteHeader = true

	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(b []byte) (int, error) {
	w.wroteHeader = true

	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)

	return n, err
}

// ReadFrom delegates so that the sendfile path survives the wrapper.
//
// This is the subtle part of wrapping a ResponseWriter. http.ServeContent copies
// through io.Copy, which looks for io.ReaderFrom on the destination; net/http
// implements it and uses sendfile for an *os.File source. A wrapper that only
// defines Write silently removes that, turning every blob read into a userspace
// copy through this process.
func (w *responseRecorder) ReadFrom(r io.Reader) (int64, error) {
	w.wroteHeader = true

	if from, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := from.ReadFrom(r)
		w.written += n

		return n, err
	}

	// The destination is written through the embedded writer, not through w, so
	// io.Copy cannot recurse back into this method.
	n, err := io.Copy(w.ResponseWriter, r)
	w.written += n

	return n, err
}

// Unwrap exposes the underlying writer to http.ResponseController, which is how
// Flush, Hijack and the per-request deadlines keep working through the wrapper.
func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
