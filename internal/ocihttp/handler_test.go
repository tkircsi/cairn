package ocihttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"

	"github.com/tkircsi/cairn/internal/blobstore"
	"github.com/tkircsi/cairn/internal/metastore/sqlite"
	"github.com/tkircsi/cairn/internal/ocihttp"
	"github.com/tkircsi/cairn/internal/registry"
	"github.com/tkircsi/cairn/internal/uploadstore"
)

const repo = "acme/widgets"

// helloDigest is written out rather than computed so that at least one test
// asserts against a digest produced outside this program. Everything else
// derives digests from the fixtures, which would keep passing if the hashing were
// wrong in a self-consistent way.
const helloDigest = digest.Digest("sha256:0da5290841b9d348bcd992cdae451553b669f437bda5ec3eeacddbf7a3673524")

func newServer(t *testing.T, opts ...registry.Option) *httptest.Server {
	t.Helper()

	dir := t.TempDir()

	blobs, err := blobstore.NewFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	uploads, err := uploadstore.NewFS(filepath.Join(dir, "uploads"))
	if err != nil {
		t.Fatalf("upload store: %v", err)
	}

	meta, err := sqlite.Open(context.Background(), filepath.Join(dir, "cairn.db"))
	if err != nil {
		t.Fatalf("metastore: %v", err)
	}

	t.Cleanup(func() { meta.Close() })

	server := httptest.NewServer(ocihttp.NewHandler(registry.New(blobs, uploads, meta, opts...)))
	t.Cleanup(server.Close)

	return server
}

func fixture(t *testing.T, name string) ([]byte, digest.Digest) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return data, digest.FromBytes(data)
}

// do issues a request and leaves the response open for the caller to inspect.
func do(
	t *testing.T,
	server *httptest.Server,
	method, path string,
	body io.Reader,
	headers map[string]string,
) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, server.URL+path, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}

	t.Cleanup(func() { resp.Body.Close() })

	return resp
}

// errorCode reads the spec's error envelope, which clients switch on.
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()

	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}

	if len(body.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(body.Errors))
	}

	return body.Errors[0].Code
}

// push uploads data in one request and returns its digest.
func push(t *testing.T, server *httptest.Server, repository string, data []byte, dgst digest.Digest) {
	t.Helper()

	path := fmt.Sprintf("/v2/%s/blobs/uploads/?digest=%s", repository, dgst)

	resp := do(t, server, http.MethodPost, path, bytes.NewReader(data), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}
}

func TestAPIVersion(t *testing.T) {
	server := newServer(t)

	resp := do(t, server, http.MethodGet, "/v2/", nil, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Clients use this header to tell a registry from any other server that
	// happens to answer /v2/ with a 200.
	if got := resp.Header.Get("Docker-Distribution-API-Version"); got != "registry/2.0" {
		t.Errorf("API version header = %q, want registry/2.0", got)
	}

	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "{}" {
		t.Errorf("body = %q, want {}", body)
	}
}

// TestMonolithicPush covers end-4b and end-2: the whole blob in one POST, then
// read it back.
func TestMonolithicPush(t *testing.T) {
	server := newServer(t)
	data, dgst := fixture(t, "hello.txt")

	if dgst != helloDigest {
		t.Fatalf("fixture digest = %s, want %s", dgst, helloDigest)
	}

	resp := do(t, server, http.MethodPost,
		"/v2/"+repo+"/blobs/uploads/?digest="+dgst.String(), bytes.NewReader(data), nil)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	// Location is how a client learns where the blob now lives; without it a
	// single-request push tells it nothing it can use.
	want := "/v2/" + repo + "/blobs/" + dgst.String()
	if got := resp.Header.Get("Location"); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	if got := resp.Header.Get("Docker-Content-Digest"); got != dgst.String() {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, dgst)
	}

	t.Run("GET returns the bytes", func(t *testing.T) {
		resp := do(t, server, http.MethodGet, "/v2/"+repo+"/blobs/"+dgst.String(), nil, nil)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		got, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(got, data) {
			t.Errorf("body = %q, want %q", got, data)
		}

		if got := resp.Header.Get("Docker-Content-Digest"); got != dgst.String() {
			t.Errorf("Docker-Content-Digest = %q, want %q", got, dgst)
		}
	})

	t.Run("HEAD reports the size without a body", func(t *testing.T) {
		resp := do(t, server, http.MethodHead, "/v2/"+repo+"/blobs/"+dgst.String(), nil, nil)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		if resp.ContentLength != int64(len(data)) {
			t.Errorf("Content-Length = %d, want %d", resp.ContentLength, len(data))
		}

		body, _ := io.ReadAll(resp.Body)
		if len(body) != 0 {
			t.Errorf("HEAD returned %d bytes of body", len(body))
		}
	})
}

// TestChunkedPush walks the full session: end-4a, end-5 twice, end-13, end-6.
func TestChunkedPush(t *testing.T) {
	server := newServer(t)
	data, dgst := fixture(t, "layer.txt")

	resp := do(t, server, http.MethodPost, "/v2/"+repo+"/blobs/uploads/", nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202", resp.StatusCode)
	}

	session := resp.Header.Get("Location")
	if session == "" {
		t.Fatal("start returned no Location")
	}

	// An empty session still has to report a Range, or a resuming client cannot
	// tell "nothing received" from "header not supported".
	if got := resp.Header.Get("Range"); got != "0-0" {
		t.Errorf("Range = %q, want 0-0", got)
	}

	// end-4a: "The <location> MUST contain a UUID representing a unique session
	// ID for the upload to follow."
	sessionID := resp.Header.Get("Docker-Upload-UUID")

	parsed, err := uuid.Parse(sessionID)
	if err != nil {
		t.Errorf("session id %q is not a UUID: %v", sessionID, err)
	} else if parsed.Version() != 4 {
		t.Errorf("session id is UUID version %d, want 4: a guessable session id is a capability leak", parsed.Version())
	}

	if !strings.HasSuffix(session, sessionID) {
		t.Errorf("Location %q does not contain the session id %q", session, sessionID)
	}

	// Split at an offset that is not a round number, so an off-by-one in the
	// offset arithmetic cannot pass by coincidence.
	const split = 3001

	chunks := [][]byte{data[:split], data[split:]}
	sent := 0

	for i, chunk := range chunks {
		headers := map[string]string{
			"Content-Range": fmt.Sprintf("%d-%d", sent, sent+len(chunk)-1),
			"Content-Type":  "application/octet-stream",
		}

		resp := do(t, server, http.MethodPatch, session, bytes.NewReader(chunk), headers)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("chunk %d status = %d, want 202: %s", i, resp.StatusCode, errorCode(t, resp))
		}

		sent += len(chunk)

		want := fmt.Sprintf("0-%d", sent-1)
		if got := resp.Header.Get("Range"); got != want {
			t.Errorf("chunk %d Range = %q, want %q", i, got, want)
		}
	}

	t.Run("status reports the offset mid-upload", func(t *testing.T) {
		resp := do(t, server, http.MethodGet, session, nil, nil)

		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", resp.StatusCode)
		}

		want := fmt.Sprintf("0-%d", len(data)-1)
		if got := resp.Header.Get("Range"); got != want {
			t.Errorf("Range = %q, want %q", got, want)
		}
	})

	resp = do(t, server, http.MethodPut, session+"?digest="+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("close status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	t.Run("reassembled blob matches the fixture", func(t *testing.T) {
		resp := do(t, server, http.MethodGet, "/v2/"+repo+"/blobs/"+dgst.String(), nil, nil)

		// The read error is checked, not discarded. A truncated body and a wrong
		// body both end up as a length mismatch otherwise, and they have entirely
		// different causes.
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v (got %d of %d bytes)", err, len(got), len(data))
		}

		if !bytes.Equal(got, data) {
			t.Errorf("blob differs from fixture: got %d bytes, want %d", len(got), len(data))
		}
	})

	t.Run("session is gone once committed", func(t *testing.T) {
		resp := do(t, server, http.MethodGet, session, nil, nil)

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}

		if code := errorCode(t, resp); code != "BLOB_UPLOAD_UNKNOWN" {
			t.Errorf("code = %s, want BLOB_UPLOAD_UNKNOWN", code)
		}
	})
}

// TestNonContiguousChunkIsRefused is the case that makes a silent corruption
// otherwise: a chunk that does not start where the last one ended would leave a
// gap in the middle of the blob.
func TestNonContiguousChunkIsRefused(t *testing.T) {
	server := newServer(t)
	data, _ := fixture(t, "layer.txt")

	resp := do(t, server, http.MethodPost, "/v2/"+repo+"/blobs/uploads/", nil, nil)
	session := resp.Header.Get("Location")

	first := data[:100]

	resp = do(t, server, http.MethodPatch, session, bytes.NewReader(first),
		map[string]string{"Content-Range": "0-99"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first chunk status = %d, want 202", resp.StatusCode)
	}

	// Claims to start at 200 when the session holds 100 bytes.
	resp = do(t, server, http.MethodPatch, session, bytes.NewReader(data[200:300]),
		map[string]string{"Content-Range": "200-299"})

	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", resp.StatusCode)
	}

	if code := errorCode(t, resp); code != "BLOB_UPLOAD_INVALID" {
		t.Errorf("code = %s, want BLOB_UPLOAD_INVALID", code)
	}

	t.Run("offset is unchanged by the rejected chunk", func(t *testing.T) {
		resp := do(t, server, http.MethodGet, session, nil, nil)

		if got := resp.Header.Get("Range"); got != "0-99" {
			t.Errorf("Range = %q, want 0-99: the refused chunk was partly written", got)
		}
	})
}

// TestDigestMismatchIsRefused checks the one guarantee a content-addressed store
// exists to provide: what you can read back is what the digest says it is.
func TestDigestMismatchIsRefused(t *testing.T) {
	server := newServer(t)
	data, _ := fixture(t, "hello.txt")

	wrong := digest.FromString("some other content entirely")

	resp := do(t, server, http.MethodPost,
		"/v2/"+repo+"/blobs/uploads/?digest="+wrong.String(), bytes.NewReader(data), nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	if code := errorCode(t, resp); code != "DIGEST_INVALID" {
		t.Errorf("code = %s, want DIGEST_INVALID", code)
	}

	t.Run("nothing became readable at the claimed digest", func(t *testing.T) {
		resp := do(t, server, http.MethodGet, "/v2/"+repo+"/blobs/"+wrong.String(), nil, nil)

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("nor at the real digest, which was never granted", func(t *testing.T) {
		resp := do(t, server, http.MethodGet, "/v2/"+repo+"/blobs/"+helloDigest.String(), nil, nil)

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

// TestRangeRequest covers the SHOULD in end-2 that makes a resumable pull
// possible.
func TestRangeRequest(t *testing.T) {
	server := newServer(t)
	data, dgst := fixture(t, "layer.txt")
	push(t, server, repo, data, dgst)

	resp := do(t, server, http.MethodGet, "/v2/"+repo+"/blobs/"+dgst.String(), nil,
		map[string]string{"Range": "bytes=100-199"})

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}

	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, data[100:200]) {
		t.Errorf("body = %q, want %q", got, data[100:200])
	}
}

// TestBlobsAreScopedToRepository is why a content-addressed store still needs an
// index: the bytes are shared, the right to read them is not.
func TestBlobsAreScopedToRepository(t *testing.T) {
	server := newServer(t)
	data, dgst := fixture(t, "hello.txt")
	push(t, server, repo, data, dgst)

	resp := do(t, server, http.MethodGet, "/v2/other/repo/blobs/"+dgst.String(), nil, nil)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: a blob leaked across repositories", resp.StatusCode)
	}

	if code := errorCode(t, resp); code != "BLOB_UNKNOWN" {
		t.Errorf("code = %s, want BLOB_UNKNOWN", code)
	}
}

// TestMount covers end-11 and its fallback, which is the endpoint the membership
// table pays for.
func TestMount(t *testing.T) {
	server := newServer(t)
	data, dgst := fixture(t, "config.json")
	push(t, server, repo, data, dgst)

	t.Run("mountable blob is granted without a transfer", func(t *testing.T) {
		path := fmt.Sprintf("/v2/other/repo/blobs/uploads/?mount=%s&from=%s", dgst, repo)

		resp := do(t, server, http.MethodPost, path, nil, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}

		want := "/v2/other/repo/blobs/" + dgst.String()
		if got := resp.Header.Get("Location"); got != want {
			t.Errorf("Location = %q, want %q", got, want)
		}

		resp = do(t, server, http.MethodGet, "/v2/other/repo/blobs/"+dgst.String(), nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("mounted blob status = %d, want 200", resp.StatusCode)
		}

		got, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(got, data) {
			t.Errorf("mounted bytes differ from the original")
		}
	})

	t.Run("unmountable blob falls back to a session", func(t *testing.T) {
		absent := digest.FromString("nobody has these bytes")
		path := fmt.Sprintf("/v2/other/repo/blobs/uploads/?mount=%s&from=%s", absent, repo)

		// 202 rather than an error: the spec turns a failed mount into an ordinary
		// upload so the client can just send the bytes it already holds.
		resp := do(t, server, http.MethodPost, path, nil, nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}

		if resp.Header.Get("Location") == "" {
			t.Error("fallback returned no session Location")
		}
	})
}

func TestCancelUpload(t *testing.T) {
	server := newServer(t)
	data, _ := fixture(t, "hello.txt")

	resp := do(t, server, http.MethodPost, "/v2/"+repo+"/blobs/uploads/", nil, nil)
	session := resp.Header.Get("Location")

	resp = do(t, server, http.MethodPatch, session, bytes.NewReader(data), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chunk status = %d, want 202", resp.StatusCode)
	}

	resp = do(t, server, http.MethodDelete, session, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel status = %d, want 204", resp.StatusCode)
	}

	resp = do(t, server, http.MethodGet, session, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status after cancel = %d, want 404", resp.StatusCode)
	}
}

func TestDeleteBlob(t *testing.T) {
	server := newServer(t)
	data, dgst := fixture(t, "hello.txt")
	push(t, server, repo, data, dgst)

	// 202, not 204: the membership row goes now, the bytes are reclaimed by a
	// sweep that has not happened yet.
	resp := do(t, server, http.MethodDelete, "/v2/"+repo+"/blobs/"+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	resp = do(t, server, http.MethodGet, "/v2/"+repo+"/blobs/"+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status after delete = %d, want 404", resp.StatusCode)
	}
}

func TestMaxBlobSize(t *testing.T) {
	data, dgst := fixture(t, "layer.txt")

	server := newServer(t, registry.WithMaxBlobSize(int64(len(data)/2)))

	resp := do(t, server, http.MethodPost,
		"/v2/"+repo+"/blobs/uploads/?digest="+dgst.String(), bytes.NewReader(data), nil)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}

	if code := errorCode(t, resp); code != "SIZE_INVALID" {
		t.Errorf("code = %s, want SIZE_INVALID", code)
	}
}

func TestRejectedRequests(t *testing.T) {
	server := newServer(t)

	// A well-formed but unissued session id: a valid v4 UUID that no upload ever
	// used. This has to be a 404 and not a new session, or a client could choose
	// its own upload identifiers.
	const unissued = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

	cases := []struct {
		name   string
		method string
		path   string
		status int
		code   string
	}{
		{
			name:   "malformed digest on read",
			method: http.MethodGet,
			path:   "/v2/" + repo + "/blobs/not-a-digest",
			status: http.StatusBadRequest,
			code:   "DIGEST_INVALID",
		},
		{
			name:   "uppercase repository name",
			method: http.MethodGet,
			path:   "/v2/ACME/widgets/blobs/" + helloDigest.String(),
			status: http.StatusBadRequest,
			code:   "NAME_INVALID",
		},
		{
			name:   "close without a digest",
			method: http.MethodPut,
			path:   "/v2/" + repo + "/blobs/uploads/" + unissued,
			status: http.StatusBadRequest,
			code:   "DIGEST_INVALID",
		},
		{
			name:   "unissued session",
			method: http.MethodPatch,
			path:   "/v2/" + repo + "/blobs/uploads/" + unissued,
			status: http.StatusNotFound,
			code:   "BLOB_UPLOAD_UNKNOWN",
		},
		{
			name:   "session id that could not have been issued",
			method: http.MethodPatch,
			path:   "/v2/" + repo + "/blobs/uploads/../../etc/passwd",
			status: http.StatusNotFound,
			code:   "BLOB_UPLOAD_UNKNOWN",
		},
		{
			// The same UUID unhyphenated. uuid.Parse accepts this spelling, so
			// without the canonical-form check it would reach the filesystem as a
			// name this package never issued.
			name:   "non-canonical spelling of a UUID",
			method: http.MethodPatch,
			path:   "/v2/" + repo + "/blobs/uploads/3f2504e04f8941d39a0c0305e82c3301",
			status: http.StatusNotFound,
			code:   "BLOB_UPLOAD_UNKNOWN",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, server, tc.method, tc.path, nil, nil)

			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}

			if code := errorCode(t, resp); code != tc.code {
				t.Errorf("code = %s, want %s", code, tc.code)
			}
		})
	}
}
