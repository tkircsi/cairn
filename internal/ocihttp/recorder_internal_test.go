package ocihttp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readerFromWriter stands in for net/http's own ResponseWriter, which implements
// io.ReaderFrom and is how a copy reaches sendfile.
type readerFromWriter struct {
	http.ResponseWriter

	used bool
}

func (w *readerFromWriter) ReadFrom(r io.Reader) (int64, error) {
	w.used = true

	return io.Copy(w.ResponseWriter, r)
}

func TestRecorderDelegatesReadFrom(t *testing.T) {
	inner := &readerFromWriter{ResponseWriter: httptest.NewRecorder()}
	recorder := &responseRecorder{ResponseWriter: inner, status: http.StatusOK}

	n, err := recorder.ReadFrom(strings.NewReader("0123456789"))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}

	if !inner.used {
		t.Error("ReadFrom did not delegate: the sendfile path is lost")
	}

	if n != 10 || recorder.written != 10 {
		t.Errorf("n = %d, written = %d, want 10 and 10", n, recorder.written)
	}
}

// TestRecorderReadFromFallback covers a writer that is not an io.ReaderFrom.
// httptest.ResponseRecorder is one, so this is not a hypothetical path.
func TestRecorderReadFromFallback(t *testing.T) {
	inner := httptest.NewRecorder()

	if _, ok := http.ResponseWriter(inner).(io.ReaderFrom); ok {
		t.Skip("httptest.ResponseRecorder now implements io.ReaderFrom")
	}

	recorder := &responseRecorder{ResponseWriter: inner, status: http.StatusOK}

	n, err := recorder.ReadFrom(strings.NewReader("abc"))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}

	if got := inner.Body.String(); got != "abc" {
		t.Errorf("body = %q, want %q", got, "abc")
	}

	if n != 3 || recorder.written != 3 {
		t.Errorf("n = %d, written = %d, want 3 and 3", n, recorder.written)
	}
}

// TestRecorderKeepsFirstStatus matters because the handler writes an error status
// and then a body; a recorder that took the last value would log the wrong code.
func TestRecorderKeepsFirstStatus(t *testing.T) {
	recorder := &responseRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}

	recorder.WriteHeader(http.StatusNotFound)
	recorder.WriteHeader(http.StatusOK)

	if recorder.status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", recorder.status)
	}
}

// TestRecorderDefaultsToOK reflects net/http: a handler that writes a body
// without calling WriteHeader has implicitly sent a 200.
func TestRecorderDefaultsToOK(t *testing.T) {
	recorder := &responseRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}

	if _, err := recorder.Write([]byte("body")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if recorder.status != http.StatusOK {
		t.Errorf("status = %d, want 200", recorder.status)
	}

	if recorder.written != 4 {
		t.Errorf("written = %d, want 4", recorder.written)
	}
}
