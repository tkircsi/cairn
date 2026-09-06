package ocihttp_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tkircsi/cairn/internal/logging"
	"github.com/tkircsi/cairn/internal/ocihttp"
)

// logged runs one request through the middleware and returns the records it
// produced, one per line.
func logged(t *testing.T, handler http.Handler, req *http.Request) []map[string]any {
	t.Helper()

	var buf bytes.Buffer

	logger, err := logging.New(&buf, logging.FormatJSON, "debug")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	ocihttp.WithLogging(handler, logger).ServeHTTP(httptest.NewRecorder(), req)

	var records []map[string]any

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}

		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}

		records = append(records, record)
	}

	return records
}

func TestAccessLogFields(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Upload-UUID", "3f2504e0-4f89-41d3-9a0c-0305e82c3301")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("hello"))
	})

	records := logged(t, handler, httptest.NewRequest(http.MethodPost, "/v2/acme/widgets/blobs/uploads/", nil))

	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}

	record := records[0]

	for field, want := range map[string]any{
		"method": "POST",
		"path":   "/v2/acme/widgets/blobs/uploads/",
		"status": float64(http.StatusAccepted),
		"bytes":  float64(5),
		"upload": "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"level":  "INFO",
	} {
		if record[field] != want {
			t.Errorf("%s = %v, want %v", field, record[field], want)
		}
	}

	if _, ok := record["duration"]; !ok {
		t.Error("record has no duration")
	}
}

// TestAccessLogLevels checks that a client's mistake and the registry breaking
// are not the same event, which is the whole reason to filter by level.
func TestAccessLogLevels(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusOK, "INFO"},
		{http.StatusAccepted, "INFO"},
		{http.StatusNotFound, "WARN"},
		{http.StatusRequestedRangeNotSatisfiable, "WARN"},
		{http.StatusInternalServerError, "ERROR"},
	} {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		})

		records := logged(t, handler, httptest.NewRequest(http.MethodGet, "/v2/", nil))

		if got := records[0]["level"]; got != tc.want {
			t.Errorf("status %d logged at %v, want %v", tc.status, got, tc.want)
		}
	}
}

// TestAccessLogResistsInjection is the reason this is structured logging and not
// a formatted line.
//
// The path is attacker-controlled. With a printf-style log, a request carrying
// CRLF plus a plausible-looking prefix would append a second entry to the log,
// letting a client write whatever it liked into the record an operator reads.
func TestAccessLogResistsInjection(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})

	// %0d%0a decodes into the path, so by the time the middleware sees it the
	// value genuinely contains a line break. No literal spaces, because
	// httptest.NewRequest parses this as a request line.
	forged := `/v2/a%0d%0a{"level":"INFO","msg":"forged","digest":"sha256:deadbeef"}`

	records := logged(t, handler, httptest.NewRequest(http.MethodGet, forged, nil))

	if len(records) != 1 {
		t.Fatalf("got %d records, want 1: the request forged a log entry", len(records))
	}

	// The bytes survive as data inside the path field, which is correct: nothing
	// is lost, it simply cannot be mistaken for structure.
	path, _ := records[0]["path"].(string)
	if !strings.Contains(path, "\r\n") {
		t.Errorf("path = %q, expected it to carry the raw line break as data", path)
	}

	if records[0]["msg"] != "request" {
		t.Errorf("msg = %v, want %q", records[0]["msg"], "request")
	}
}

func TestAccessLogTruncatesLongPaths(t *testing.T) {
	// An unbounded path would let a client choose how much disk each request
	// costs.
	long := "/v2/" + strings.Repeat("a", 4096) + "/blobs/uploads/"

	records := logged(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		httptest.NewRequest(http.MethodGet, long, nil))

	path, _ := records[0]["path"].(string)
	if len(path) >= len(long) {
		t.Errorf("path was not truncated: logged %d of %d bytes", len(path), len(long))
	}

	if !strings.HasSuffix(path, "(truncated)") {
		t.Errorf("truncated path should say so, got %q", path[max(0, len(path)-32):])
	}
}

// TestAccessLogPreservesReaderFrom guards the non-obvious cost of wrapping a
// ResponseWriter: http.ServeContent copies through io.Copy, which uses
// io.ReaderFrom on the destination to reach sendfile. A wrapper that defines only
// Write silently turns every blob read into a userspace copy.
func TestAccessLogPreservesReaderFrom(t *testing.T) {
	var wrapped http.ResponseWriter

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped = w
	})

	ocihttp.WithLogging(handler, logging.Discard()).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v2/", nil))

	if _, ok := wrapped.(io.ReaderFrom); !ok {
		t.Error("the wrapped ResponseWriter does not implement io.ReaderFrom")
	}

	// http.ResponseController reaches Flush and friends through Unwrap, so losing
	// it would break streaming for anything added later.
	type unwrapper interface{ Unwrap() http.ResponseWriter }

	if _, ok := wrapped.(unwrapper); !ok {
		t.Error("the wrapped ResponseWriter does not implement Unwrap")
	}
}
