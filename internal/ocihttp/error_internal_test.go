package ocihttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tkircsi/cairn/internal/registry"
)

// busyError is shaped the way the metastore actually wraps contention: the operation,
// the sentinel, and the driver's own message with its result code. Constructed rather
// than provoked because what is under test here is the mapping, and driving a real
// SQLITE_BUSY through an HTTP handler would test the store instead.
func busyError() error {
	return fmt.Errorf("insert manifest: %w: %w",
		registry.ErrBusy, errors.New("database is locked (5) (SQLITE_BUSY)"))
}

// TestContentionAnswers503 is the assertion that keeps the mapping from rotting.
//
// It is easy to lose by accident: the sentinel travels from the metastore through the
// registry by being wrapped rather than by being named, so any layer that replaces an
// error instead of wrapping it silently turns this back into a 500. Nothing else fails
// when that happens -- the push still errors, just uselessly -- so the guarantee has to
// be asserted rather than inferred.
func TestContentionAnswers503(t *testing.T) {
	response := httptest.NewRecorder()
	recorder := &responseRecorder{ResponseWriter: response, status: http.StatusOK}

	writeServerError(recorder, busyError())

	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: a 500 tells the client to give up on a "+
			"request that would succeed on retry", response.Code)
	}

	// Without this a compliant client has no interval to wait and either retries
	// immediately, which is what caused the contention, or not at all.
	if got := response.Header().Get("Retry-After"); got != retryAfterBusy {
		t.Errorf("Retry-After = %q, want %q", got, retryAfterBusy)
	}

	if got := errorCodeOf(t, response); got != "TOOMANYREQUESTS" {
		t.Errorf("code = %q, want TOOMANYREQUESTS", got)
	}

	// The cause still has to reach the access log, or an operator sees a 503 with no
	// way to tell contention from anything else that might answer with one.
	if recorder.fault == nil {
		t.Error("fault not recorded: the 503 leaves no trace of its cause in the log")
	}

	// And must not reach the client. The wrapped message carries SQL text, which is
	// the reason writeServerError withholds detail in the first place.
	if body := response.Body.String(); strings.Contains(body, "SQLITE_BUSY") {
		t.Errorf("response leaks the driver's message to the client: %s", body)
	}
}

// TestOtherFailuresStillAnswer500 is the half that stops the mapping being too eager.
//
// 503 with Retry-After is an instruction to try again, so applying it to a failure that
// is not transient turns one broken request into a client retrying it forever.
func TestOtherFailuresStillAnswer500(t *testing.T) {
	response := httptest.NewRecorder()
	recorder := &responseRecorder{ResponseWriter: response, status: http.StatusOK}

	writeServerError(recorder, errors.New("insert manifest: disk I/O error"))

	if response.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", response.Code)
	}

	if got := response.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q on a permanent failure, which invites an endless "+
			"retry loop", got)
	}

	if got := errorCodeOf(t, response); got != "UNSUPPORTED" {
		t.Errorf("code = %q, want UNSUPPORTED", got)
	}
}

func errorCodeOf(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()

	var body ociError
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", response.Body.String(), err)
	}

	if len(body.Errors) != 1 {
		t.Fatalf("got %d errors in the envelope, want 1", len(body.Errors))
	}

	return body.Errors[0].Code
}
