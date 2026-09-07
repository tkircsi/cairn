package ocihttp_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/tkircsi/cairn/internal/registry"
)

const (
	imageManifestType = ocispec.MediaTypeImageManifest
	imageIndexType    = ocispec.MediaTypeImageIndex
	emptyConfigType   = ocispec.MediaTypeEmptyJSON
)

// descriptorFor pushes data as a blob and returns a descriptor naming it, which
// is the ordering a manifest push requires: the leaves have to exist first.
func descriptorFor(
	t *testing.T,
	server *httptest.Server,
	repository, mediaType string,
	data []byte,
) ocispec.Descriptor {
	t.Helper()

	dgst := digest.FromBytes(data)
	push(t, server, repository, data, dgst)

	return ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    dgst,
		Size:      int64(len(data)),
	}
}

// encode marshals a document and returns it with its digest, since every manifest
// request needs both and they must agree.
func encode(t *testing.T, doc any) ([]byte, digest.Digest) {
	t.Helper()

	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return raw, digest.FromBytes(raw)
}

// putManifest issues end-7 without asserting the outcome, so a test can check
// either a success or a specific refusal.
func putManifest(
	t *testing.T,
	server *httptest.Server,
	repository string,
	dgst digest.Digest,
	mediaType string,
	raw []byte,
) *http.Response {
	t.Helper()

	return do(t, server, http.MethodPut,
		fmt.Sprintf("/v2/%s/manifests/%s", repository, dgst),
		bytes.NewReader(raw),
		map[string]string{"Content-Type": mediaType},
	)
}

// imageManifest builds a minimal but genuinely valid image manifest over content
// pushed into repository.
func imageManifest(t *testing.T, server *httptest.Server, repository string) ([]byte, digest.Digest) {
	t.Helper()

	config := descriptorFor(t, server, repository, emptyConfigType, []byte("{}"))
	layer := descriptorFor(t, server, repository,
		ocispec.MediaTypeImageLayer, []byte("a layer, more or less"))

	return encode(t, ocispec.Manifest{
		MediaType: imageManifestType,
		Config:    config,
		Layers:    []ocispec.Descriptor{layer},
	})
}

// TestManifestRoundTrip covers end-7 then end-3: push a manifest over content
// that exists, read it back byte for byte.
func TestManifestRoundTrip(t *testing.T) {
	server := newServer(t)

	raw, dgst := imageManifest(t, server, repo)

	resp := putManifest(t, server, repo, dgst, imageManifestType, raw)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	want := "/v2/" + repo + "/manifests/" + dgst.String()
	if got := resp.Header.Get("Location"); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	if got := resp.Header.Get("Docker-Content-Digest"); got != dgst.String() {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, dgst)
	}

	// A manifest with no subject must not claim one, or a client would go looking
	// for a referrers relationship that does not exist.
	if got := resp.Header.Get("OCI-Subject"); got != "" {
		t.Errorf("OCI-Subject = %q, want it absent", got)
	}

	resp = do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/"+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, errorCode(t, resp))
	}

	// The recorded media type, not a sniffed one: a client dispatches on this to
	// decide what kind of document it is holding.
	if got := resp.Header.Get("Content-Type"); got != imageManifestType {
		t.Errorf("Content-Type = %q, want %q", got, imageManifestType)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// Byte-for-byte, not merely equivalent JSON. The digest is over these exact
	// bytes, so re-serialising would silently break every signature over it.
	if !bytes.Equal(body, raw) {
		t.Errorf("body was not returned verbatim:\n got %s\nwant %s", body, raw)
	}
}

// TestManifestHeadDoesNotReadContent checks that a HEAD is answerable from the
// index alone, which is why media type and size are columns.
func TestManifestHeadDoesNotReadContent(t *testing.T) {
	server := newServer(t)

	raw, dgst := imageManifest(t, server, repo)

	if resp := putManifest(t, server, repo, dgst, imageManifestType, raw); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	resp := do(t, server, http.MethodHead, "/v2/"+repo+"/manifests/"+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", resp.StatusCode)
	}

	if resp.ContentLength != int64(len(raw)) {
		t.Errorf("Content-Length = %d, want %d", resp.ContentLength, len(raw))
	}

	if got := resp.Header.Get("Content-Type"); got != imageManifestType {
		t.Errorf("Content-Type = %q, want %q", got, imageManifestType)
	}

	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("HEAD returned %d bytes of body", len(body))
	}
}

// TestManifestRejectsMissingBlobs is the check that makes a stored manifest a
// resolvable one.
func TestManifestRejectsMissingBlobs(t *testing.T) {
	server := newServer(t)

	// Never pushed, so the manifest names content the repository cannot serve.
	absent := digest.FromString("a layer that was never uploaded")

	raw, dgst := encode(t, ocispec.Manifest{
		MediaType: imageManifestType,
		Config:    ocispec.Descriptor{MediaType: emptyConfigType, Digest: digest.FromString("{}"), Size: 2},
		Layers:    []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayer, Digest: absent, Size: 1}},
	})

	resp := putManifest(t, server, repo, dgst, imageManifestType, raw)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	if code := errorCode(t, resp); code != "MANIFEST_BLOB_UNKNOWN" {
		t.Errorf("code = %s, want MANIFEST_BLOB_UNKNOWN", code)
	}

	// And it must not have been stored despite the refusal.
	resp = do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/"+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a refused manifest was stored anyway: GET returned %d", resp.StatusCode)
	}
}

// TestManifestBlobsMustBeInTheSameRepository pins that the reference check is
// repository-scoped, not global. Otherwise a manifest could name bytes it has no
// right to read, and the mount endpoint would have no purpose.
func TestManifestBlobsMustBeInTheSameRepository(t *testing.T) {
	server := newServer(t)

	const other = "acme/elsewhere"

	config := descriptorFor(t, server, other, emptyConfigType, []byte("{}"))

	raw, dgst := encode(t, ocispec.Manifest{
		MediaType: imageManifestType,
		Config:    config,
		Layers:    []ocispec.Descriptor{},
	})

	resp := putManifest(t, server, repo, dgst, imageManifestType, raw)
	if code := errorCode(t, resp); resp.StatusCode != http.StatusNotFound || code != "MANIFEST_BLOB_UNKNOWN" {
		t.Fatalf("status = %d, code = %s, want 404 MANIFEST_BLOB_UNKNOWN", resp.StatusCode, code)
	}

	// Mounting it across is exactly the remedy, and the same push then succeeds.
	mount := fmt.Sprintf("/v2/%s/blobs/uploads/?mount=%s&from=%s", repo, config.Digest, other)
	if got := do(t, server, http.MethodPost, mount, nil, nil); got.StatusCode != http.StatusCreated {
		t.Fatalf("mount status = %d, want 201", got.StatusCode)
	}

	resp = putManifest(t, server, repo, dgst, imageManifestType, raw)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("after mounting, status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}
}

// TestManifestIndexRequiresItsChildren covers the other namespace: an index names
// manifests, and those are checked as manifests rather than as blobs.
func TestManifestIndexRequiresItsChildren(t *testing.T) {
	server := newServer(t)

	childRaw, childDigest := imageManifest(t, server, repo)

	indexRaw, indexDigest := encode(t, ocispec.Index{
		MediaType: imageIndexType,
		Manifests: []ocispec.Descriptor{{
			MediaType: imageManifestType,
			Digest:    childDigest,
			Size:      int64(len(childRaw)),
		}},
	})

	// The child's bytes are in the content store as of the failed push below only
	// if it was pushed as a manifest -- which it has not been yet.
	resp := putManifest(t, server, repo, indexDigest, imageIndexType, indexRaw)
	if code := errorCode(t, resp); resp.StatusCode != http.StatusNotFound || code != "MANIFEST_BLOB_UNKNOWN" {
		t.Fatalf("status = %d, code = %s, want 404 MANIFEST_BLOB_UNKNOWN", resp.StatusCode, code)
	}

	if got := putManifest(t, server, repo, childDigest, imageManifestType, childRaw); got.StatusCode != http.StatusCreated {
		t.Fatalf("child PUT status = %d, want 201: %s", got.StatusCode, errorCode(t, got))
	}

	resp = putManifest(t, server, repo, indexDigest, imageIndexType, indexRaw)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("index PUT status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}
}

// TestManifestSubjectIsRecordedButNotRequired covers the referrers ordering the
// spec permits: a signature may be pushed for a subject that is absent.
func TestManifestSubjectIsRecordedButNotRequired(t *testing.T) {
	server := newServer(t)

	const artifactType = "application/vnd.example.signature.v1+json"

	// Deliberately never pushed. A signing tool routinely signs by digest before,
	// or without, the subject being present here.
	subject := digest.FromString("a subject this registry has never seen")

	config := descriptorFor(t, server, repo, emptyConfigType, []byte("{}"))

	raw, dgst := encode(t, ocispec.Manifest{
		MediaType:    imageManifestType,
		ArtifactType: artifactType,
		Config:       config,
		Layers:       []ocispec.Descriptor{},
		Subject:      &ocispec.Descriptor{MediaType: imageManifestType, Digest: subject, Size: 99},
	})

	resp := putManifest(t, server, repo, dgst, imageManifestType, raw)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	// OCI-Subject is how a client learns the pointer was understood, and therefore
	// whether a referrers query will find it rather than the tag fallback.
	if got := resp.Header.Get("OCI-Subject"); got != subject.String() {
		t.Errorf("OCI-Subject = %q, want %q", got, subject)
	}
}

func TestManifestDigestMismatchIsRefused(t *testing.T) {
	server := newServer(t)

	raw, _ := imageManifest(t, server, repo)

	wrong := digest.FromString("not the digest of that manifest")

	resp := putManifest(t, server, repo, wrong, imageManifestType, raw)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	if code := errorCode(t, resp); code != "DIGEST_INVALID" {
		t.Errorf("code = %s, want DIGEST_INVALID", code)
	}
}

// TestManifestInvalidDocuments covers the shapes that must not be stored.
func TestManifestInvalidDocuments(t *testing.T) {
	server := newServer(t)

	config := descriptorFor(t, server, repo, emptyConfigType, []byte("{}"))

	valid := ocispec.Manifest{MediaType: imageManifestType, Config: config}

	empty, _ := encode(t, map[string]any{"mediaType": imageManifestType})
	validRaw, _ := encode(t, valid)

	cases := []struct {
		name        string
		raw         []byte
		contentType string
	}{
		{"not JSON", []byte("this is not a manifest"), imageManifestType},
		// A document without mediaType is *not* here: the spec makes it a SHOULD,
		// and the Content-Type header can supply it. See
		// TestManifestMediaTypeFallsBackToContentType.
		{"nothing referenced", empty, imageManifestType},
		// The envelope and the document describe the same bytes, so a disagreement
		// is a client bug, and which one something downstream believes is not
		// knowable from here.
		{"Content-Type disagrees with mediaType", validRaw, imageIndexType},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := putManifest(t, server, repo, digest.FromBytes(tc.raw), tc.contentType, tc.raw)

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}

			if code := errorCode(t, resp); code != "MANIFEST_INVALID" {
				t.Errorf("code = %s, want MANIFEST_INVALID", code)
			}
		})
	}
}

// TestManifestContentTypeParameters covers the spec's instruction that a registry
// SHOULD ignore parameters on Content-Type. A conformant client may append a
// charset, and a plain string comparison against mediaType would refuse it.
func TestManifestContentTypeParameters(t *testing.T) {
	server := newServer(t)

	raw, dgst := imageManifest(t, server, repo)

	resp := putManifest(t, server, repo, dgst, imageManifestType+"; charset=utf-8", raw)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	// The parameter must not survive into what is served back.
	resp = do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/"+dgst.String(), nil, nil)
	if got := resp.Header.Get("Content-Type"); got != imageManifestType {
		t.Errorf("Content-Type = %q, want %q", got, imageManifestType)
	}
}

// TestManifestMediaTypeFallsBackToContentType covers mediaType being a SHOULD
// rather than a MUST: a document without one is not malformed, and the header can
// supply what a GET has to answer with.
func TestManifestMediaTypeFallsBackToContentType(t *testing.T) {
	server := newServer(t)

	config := descriptorFor(t, server, repo, emptyConfigType, []byte("{}"))

	raw, dgst := encode(t, map[string]any{
		"schemaVersion": 2,
		"config":        config,
		"layers":        []ocispec.Descriptor{},
	})

	resp := putManifest(t, server, repo, dgst, imageManifestType, raw)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	resp = do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/"+dgst.String(), nil, nil)
	if got := resp.Header.Get("Content-Type"); got != imageManifestType {
		t.Errorf("Content-Type = %q, want the header's value %q", got, imageManifestType)
	}

	// With neither source there is nothing to record, and that is a refusal.
	bare, bareDigest := encode(t, map[string]any{"schemaVersion": 2, "config": config})

	resp = do(t, server, http.MethodPut,
		fmt.Sprintf("/v2/%s/manifests/%s", repo, bareDigest), bytes.NewReader(bare), nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("with no media type at all, status = %d, want 400", resp.StatusCode)
	}

	if code := errorCode(t, resp); code != "MANIFEST_INVALID" {
		t.Errorf("code = %s, want MANIFEST_INVALID", code)
	}
}

func TestManifestMalformedContentType(t *testing.T) {
	server := newServer(t)

	raw, dgst := imageManifest(t, server, repo)

	// Refused rather than ignored: being lenient about a header that will not parse
	// while refusing one that merely disagrees would be incoherent.
	resp := putManifest(t, server, repo, dgst, "application/;;;", raw)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	if code := errorCode(t, resp); code != "MANIFEST_INVALID" {
		t.Errorf("code = %s, want MANIFEST_INVALID", code)
	}
}

// TestMissingBlobErrorIsActionable checks that "nowhere" and "elsewhere" are
// distinguished, since only the second has a cheap remedy.
func TestMissingBlobErrorIsActionable(t *testing.T) {
	server := newServer(t)

	const other = "acme/elsewhere"

	config := descriptorFor(t, server, other, emptyConfigType, []byte("{}"))

	raw, dgst := encode(t, ocispec.Manifest{
		MediaType: imageManifestType,
		Config:    config,
		Layers:    []ocispec.Descriptor{},
	})

	message := errorMessage(t, putManifest(t, server, repo, dgst, imageManifestType, raw))

	if !strings.Contains(message, "mount") {
		t.Errorf("error does not suggest mounting: %q", message)
	}

	// The source repository must not be named: with authentication that would be a
	// disclosure, and a mount with no "from" resolves it anyway.
	if strings.Contains(message, other) {
		t.Errorf("error names the source repository %q: %q", other, message)
	}

	// A digest held nowhere gets the plain message, with nothing to suggest.
	absent := digest.FromString("bytes no repository holds")

	raw, dgst = encode(t, ocispec.Manifest{
		MediaType: imageManifestType,
		Config:    ocispec.Descriptor{MediaType: emptyConfigType, Digest: absent, Size: 1},
	})

	message = errorMessage(t, putManifest(t, server, repo, dgst, imageManifestType, raw))

	if strings.Contains(message, "mount") {
		t.Errorf("error suggests mounting content nothing holds: %q", message)
	}
}

// TestManifestIsNotABlob pins the namespace split: the two share a content store
// but not addressing, so a manifest digest is not fetchable as a blob.
func TestManifestIsNotABlob(t *testing.T) {
	server := newServer(t)

	raw, dgst := imageManifest(t, server, repo)

	if resp := putManifest(t, server, repo, dgst, imageManifestType, raw); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	resp := do(t, server, http.MethodGet, "/v2/"+repo+"/blobs/"+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("manifest was readable as a blob: status = %d, want 404", resp.StatusCode)
	}

	if code := errorCode(t, resp); code != "BLOB_UNKNOWN" {
		t.Errorf("code = %s, want BLOB_UNKNOWN", code)
	}
}

// TestManifestsAreScopedToRepository is the manifest counterpart of the blob
// scoping rule.
func TestManifestsAreScopedToRepository(t *testing.T) {
	server := newServer(t)

	raw, dgst := imageManifest(t, server, repo)

	if resp := putManifest(t, server, repo, dgst, imageManifestType, raw); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	resp := do(t, server, http.MethodGet, "/v2/acme/other/manifests/"+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}

	if code := errorCode(t, resp); code != "MANIFEST_UNKNOWN" {
		t.Errorf("code = %s, want MANIFEST_UNKNOWN", code)
	}
}

func TestDeleteManifest(t *testing.T) {
	server := newServer(t)

	raw, dgst := imageManifest(t, server, repo)

	if resp := putManifest(t, server, repo, dgst, imageManifestType, raw); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	// 202, not 204: the row is gone but the bytes are not, so the deletion really
	// is only accepted.
	resp := do(t, server, http.MethodDelete, "/v2/"+repo+"/manifests/"+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE status = %d, want 202", resp.StatusCode)
	}

	resp = do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/"+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("manifest still readable after delete: %d", resp.StatusCode)
	}

	// Deleting it twice is a 404, not a silent success: a client retrying needs to
	// be able to tell that it acted on nothing.
	resp = do(t, server, http.MethodDelete, "/v2/"+repo+"/manifests/"+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second DELETE status = %d, want 404", resp.StatusCode)
	}
}

// TestManifestTagIsRefusedNotFaked covers the scope boundary. A tag is well
// formed and this registry simply does not serve it, so saying UNSUPPORTED is
// honest where a 404 would invite the client to push over it.
func TestManifestTagIsRefusedNotFaked(t *testing.T) {
	server := newServer(t)

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete} {
		resp := do(t, server, method, "/v2/"+repo+"/manifests/v1.0.0", nil, nil)

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", method, resp.StatusCode)

			continue
		}

		if method == http.MethodHead {
			continue // no body to inspect
		}

		if code := errorCode(t, resp); code != "UNSUPPORTED" {
			t.Errorf("%s code = %s, want UNSUPPORTED", method, code)
		}
	}
}

func TestManifestRejectedReferences(t *testing.T) {
	server := newServer(t)

	cases := []struct {
		name      string
		reference string
		code      string
	}{
		{"not a digest or a tag", "sha256:short", "MANIFEST_INVALID"},
		{"unknown algorithm", "md5:d41d8cd98f00b204e9800998ecf8427e", "UNSUPPORTED"},
		{"empty-ish", ".", "MANIFEST_INVALID"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/"+tc.reference, nil, nil)

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}

			if code := errorCode(t, resp); code != tc.code {
				t.Errorf("code = %s, want %s", code, tc.code)
			}
		})
	}
}

func TestMaxManifestSize(t *testing.T) {
	server := newServer(t, registry.WithMaxManifestSize(64))

	raw, dgst := imageManifest(t, server, repo)

	if int64(len(raw)) <= 64 {
		t.Fatalf("fixture manifest is only %d bytes, so the limit is untested", len(raw))
	}

	resp := putManifest(t, server, repo, dgst, imageManifestType, raw)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}

	if code := errorCode(t, resp); code != "SIZE_INVALID" {
		t.Errorf("code = %s, want SIZE_INVALID", code)
	}
}

// TestRepositoryNamedManifests covers the routing ambiguity: a repository may
// legally end in a component called "manifests".
func TestRepositoryNamedManifests(t *testing.T) {
	server := newServer(t)

	const name = "acme/manifests"

	raw, dgst := imageManifest(t, server, name)

	if resp := putManifest(t, server, name, dgst, imageManifestType, raw); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	resp := do(t, server, http.MethodGet, "/v2/"+name+"/manifests/"+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, errorCode(t, resp))
	}
}
