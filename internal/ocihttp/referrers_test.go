package ocihttp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// referrer pushes an image manifest that names subject and returns its digest.
//
// artifactType may be empty, which is how the spec's fallback to the config media
// type gets exercised.
func referrer(
	t *testing.T,
	server *httptest.Server,
	repository string,
	subject digest.Digest,
	artifactType string,
	annotations map[string]string,
) digest.Digest {
	t.Helper()

	// A distinct config per referrer, so two referrers of the same subject are two
	// digests. Identical bytes would be one manifest pushed twice, and a test
	// checking that both are listed would pass for the wrong reason.
	config := descriptorFor(t, server, repository, emptyConfigType,
		[]byte(fmt.Sprintf(`{"artifactType":%q,"n":%d}`, artifactType, len(annotations))))

	raw, dgst := encode(t, ocispec.Manifest{
		MediaType:    imageManifestType,
		ArtifactType: artifactType,
		Config:       config,
		Layers:       []ocispec.Descriptor{},
		Subject: &ocispec.Descriptor{
			MediaType: imageManifestType,
			Digest:    subject,
			Size:      99,
		},
		Annotations: annotations,
	})

	resp := putManifest(t, server, repository, dgst, imageManifestType, raw)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push referrer: status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	return dgst
}

// getReferrers issues end-12 and decodes the index, failing on any status but 200.
func getReferrers(
	t *testing.T,
	server *httptest.Server,
	repository string,
	subject digest.Digest,
	query string,
) (ocispec.Index, *http.Response) {
	t.Helper()

	path := fmt.Sprintf("/v2/%s/referrers/%s%s", repository, subject, query)

	resp := do(t, server, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200: %s", path, resp.StatusCode, errorCode(t, resp))
	}

	var index ocispec.Index
	if err := json.NewDecoder(resp.Body).Decode(&index); err != nil {
		t.Fatalf("decode index: %v", err)
	}

	return index, resp
}

// digests reduces an index to the set of digests it lists, since most assertions
// are about membership and order is not part of the contract.
func digestsOf(index ocispec.Index) map[digest.Digest]ocispec.Descriptor {
	found := make(map[digest.Digest]ocispec.Descriptor, len(index.Manifests))

	for _, d := range index.Manifests {
		found[d.Digest] = d
	}

	return found
}

// TestReferrersRoundTrip covers end-12a: two referrers of one subject come back as
// an image index whose descriptors carry enough for a client to choose between
// them without fetching either.
func TestReferrersRoundTrip(t *testing.T) {
	server := newServer(t)

	subjectRaw, subject := imageManifest(t, server, repo)
	if resp := putManifest(t, server, repo, subject, imageManifestType, subjectRaw); resp.StatusCode != http.StatusCreated {
		t.Fatalf("push subject: status = %d, want 201", resp.StatusCode)
	}

	const (
		signatureType = "application/vnd.example.signature.v1+json"
		sbomType      = "application/vnd.example.sbom.v1+json"
	)

	signature := referrer(t, server, repo, subject, signatureType,
		map[string]string{"org.example.signer": "alice"})
	sbom := referrer(t, server, repo, subject, sbomType, nil)

	index, resp := getReferrers(t, server, repo, subject, "")

	// The spec fixes this, and clients dispatch on it. A generic application/json
	// would be read as "not an index".
	if got := resp.Header.Get("Content-Type"); got != imageIndexType {
		t.Errorf("Content-Type = %q, want %q", got, imageIndexType)
	}

	if index.MediaType != imageIndexType {
		t.Errorf("index mediaType = %q, want %q", index.MediaType, imageIndexType)
	}

	if index.SchemaVersion != 2 {
		t.Errorf("schemaVersion = %d, want 2", index.SchemaVersion)
	}

	// No filter was requested, so the registry must not claim one was applied --
	// that header is how a client decides whether it still has to filter locally.
	if got := resp.Header.Get("OCI-Filters-Applied"); got != "" {
		t.Errorf("OCI-Filters-Applied = %q, want it absent", got)
	}

	found := digestsOf(index)

	if len(found) != 2 {
		t.Fatalf("got %d referrers, want 2: %v", len(found), index.Manifests)
	}

	got, ok := found[signature]
	if !ok {
		t.Fatalf("signature %s not listed", signature)
	}

	if got.ArtifactType != signatureType {
		t.Errorf("artifactType = %q, want %q", got.ArtifactType, signatureType)
	}

	if got.MediaType != imageManifestType {
		t.Errorf("mediaType = %q, want %q", got.MediaType, imageManifestType)
	}

	// Size and digest have to describe the referrer itself, because a client fetches
	// it by them. A descriptor that named the subject's size would be unusable.
	if got.Size == 0 {
		t.Error("size = 0, want the referrer's own length")
	}

	// The annotations are the point of the endpoint: this is how a client picks one
	// signature out of many without fetching them all.
	if got.Annotations["org.example.signer"] != "alice" {
		t.Errorf("annotations = %v, want org.example.signer=alice", got.Annotations)
	}

	if _, ok := found[sbom]; !ok {
		t.Errorf("sbom %s not listed", sbom)
	}
}

// TestReferrersFilter covers end-12b, including the header that says the filter was
// honoured rather than ignored.
func TestReferrersFilter(t *testing.T) {
	server := newServer(t)

	subject := digest.FromString("a subject")

	const (
		signatureType = "application/vnd.example.signature.v1+json"
		sbomType      = "application/vnd.example.sbom.v1+json"
	)

	signature := referrer(t, server, repo, subject, signatureType, nil)
	referrer(t, server, repo, subject, sbomType, nil)

	index, resp := getReferrers(t, server, repo, subject, "?artifactType="+signatureType)

	// A registry is allowed to ignore the parameter, so a client cannot assume the
	// list is narrowed. This header is the only thing that tells it.
	if got := resp.Header.Get("OCI-Filters-Applied"); got != "artifactType" {
		t.Errorf("OCI-Filters-Applied = %q, want artifactType", got)
	}

	if len(index.Manifests) != 1 {
		t.Fatalf("got %d referrers, want 1: %v", len(index.Manifests), index.Manifests)
	}

	if index.Manifests[0].Digest != signature {
		t.Errorf("digest = %s, want %s", index.Manifests[0].Digest, signature)
	}
}

// TestReferrersFilterEncoding pins that both spellings of a media type work.
//
// Nearly every artifact type ends in "+json", and RFC 3986 does not require "+" to
// be escaped in a query, so clients send it both ways. Go's own query parser reads a
// bare "+" as a space -- an HTML form convention -- which would silently filter on a
// type that cannot exist and answer "nothing refers to this".
func TestReferrersFilterEncoding(t *testing.T) {
	server := newServer(t)

	subject := digest.FromString("a subject")

	const artifactType = "application/vnd.example.sbom.v1+json"

	sbom := referrer(t, server, repo, subject, artifactType, nil)

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"literal plus", "?artifactType=application/vnd.example.sbom.v1+json"},
		{"escaped plus", "?artifactType=application/vnd.example.sbom.v1%2Bjson"},
		{"escaped slash", "?artifactType=application%2Fvnd.example.sbom.v1%2Bjson"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index, _ := getReferrers(t, server, repo, subject, tc.query)

			if len(index.Manifests) != 1 {
				t.Fatalf("got %d referrers, want 1: %v", len(index.Manifests), index.Manifests)
			}

			if index.Manifests[0].Digest != sbom {
				t.Errorf("digest = %s, want %s", index.Manifests[0].Digest, sbom)
			}
		})
	}
}

// TestReferrersRejectsMalformedFilter checks the other half: an escape that will not
// decode is refused rather than passed through, since silently filtering on the raw
// bytes would again look like "nothing refers to this".
func TestReferrersRejectsMalformedFilter(t *testing.T) {
	server := newServer(t)

	path := fmt.Sprintf("/v2/%s/referrers/%s?artifactType=%%zz", repo, digest.FromString("x"))

	resp := do(t, server, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestReferrersFilterMatchingNothing checks that a filter no referrer satisfies is
// an empty list rather than a 404. The subject exists and so does the repository;
// only the type is absent.
func TestReferrersFilterMatchingNothing(t *testing.T) {
	server := newServer(t)

	subject := digest.FromString("a subject")
	referrer(t, server, repo, subject, "application/vnd.example.signature.v1+json", nil)

	index, resp := getReferrers(t, server, repo, subject,
		"?artifactType=application/vnd.example.nothing")

	if len(index.Manifests) != 0 {
		t.Errorf("got %d referrers, want none: %v", len(index.Manifests), index.Manifests)
	}

	if got := resp.Header.Get("OCI-Filters-Applied"); got != "artifactType" {
		t.Errorf("OCI-Filters-Applied = %q, want artifactType", got)
	}
}

// TestReferrersEmptyArtifactTypeIsNotAFilter pins the reading of a present-but-empty
// parameter. Filtering on the empty string could only match nothing, so treating it
// as a filter would turn a client's sloppy URL building into a silently empty answer.
func TestReferrersEmptyArtifactTypeIsNotAFilter(t *testing.T) {
	server := newServer(t)

	subject := digest.FromString("a subject")
	referrer(t, server, repo, subject, "application/vnd.example.signature.v1+json", nil)

	index, resp := getReferrers(t, server, repo, subject, "?artifactType=")

	if len(index.Manifests) != 1 {
		t.Errorf("got %d referrers, want 1: %v", len(index.Manifests), index.Manifests)
	}

	if got := resp.Header.Get("OCI-Filters-Applied"); got != "" {
		t.Errorf("OCI-Filters-Applied = %q, want it absent when nothing was filtered", got)
	}
}

// TestReferrersArtifactTypeFallsBackToConfig covers the spec's rule for an image
// manifest that declares no artifactType: the config's media type stands in.
//
// This keeps the pre-1.1 convention working, where the artifact's type was carried
// by the config descriptor because there was no field for it.
func TestReferrersArtifactTypeFallsBackToConfig(t *testing.T) {
	server := newServer(t)

	subject := digest.FromString("a subject")

	const configType = "application/vnd.example.config.v1+json"

	config := descriptorFor(t, server, repo, configType, []byte(`{"config":true}`))

	raw, dgst := encode(t, ocispec.Manifest{
		MediaType: imageManifestType,
		// Deliberately absent.
		Config:  config,
		Layers:  []ocispec.Descriptor{},
		Subject: &ocispec.Descriptor{MediaType: imageManifestType, Digest: subject, Size: 99},
	})

	if resp := putManifest(t, server, repo, dgst, imageManifestType, raw); resp.StatusCode != http.StatusCreated {
		t.Fatalf("push referrer: status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	index, _ := getReferrers(t, server, repo, subject, "")

	if len(index.Manifests) != 1 {
		t.Fatalf("got %d referrers, want 1", len(index.Manifests))
	}

	if got := index.Manifests[0].ArtifactType; got != configType {
		t.Errorf("artifactType = %q, want the config's %q", got, configType)
	}

	// And the substituted value has to be what the filter matches, or the fallback
	// would be visible in the listing but unusable for narrowing it.
	filtered, _ := getReferrers(t, server, repo, subject, "?artifactType="+configType)

	if len(filtered.Manifests) != 1 {
		t.Errorf("filtering on the config type found %d, want 1", len(filtered.Manifests))
	}
}

// TestReferrersIndexWithoutArtifactTypeOmitsIt covers the other half of the rule.
// An index has no config, so there is nothing to fall back to and the field must be
// absent -- not present and empty, which a client would read as a real value.
func TestReferrersIndexWithoutArtifactTypeOmitsIt(t *testing.T) {
	server := newServer(t)

	subject := digest.FromString("a subject")

	childRaw, child := imageManifest(t, server, repo)
	if resp := putManifest(t, server, repo, child, imageManifestType, childRaw); resp.StatusCode != http.StatusCreated {
		t.Fatalf("push child: status = %d, want 201", resp.StatusCode)
	}

	raw, dgst := encode(t, ocispec.Index{
		MediaType: imageIndexType,
		Manifests: []ocispec.Descriptor{{
			MediaType: imageManifestType,
			Digest:    child,
			Size:      int64(len(childRaw)),
		}},
		Subject: &ocispec.Descriptor{MediaType: imageManifestType, Digest: subject, Size: 99},
	})

	if resp := putManifest(t, server, repo, dgst, imageIndexType, raw); resp.StatusCode != http.StatusCreated {
		t.Fatalf("push index referrer: status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	// Read as raw JSON rather than into a struct, because the distinction under test
	// is presence of the key -- which unmarshalling into a string erases.
	path := fmt.Sprintf("/v2/%s/referrers/%s", repo, subject)
	resp := do(t, server, http.MethodGet, path, nil, nil)

	var index struct {
		Manifests []map[string]json.RawMessage `json:"manifests"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&index); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(index.Manifests) != 1 {
		t.Fatalf("got %d referrers, want 1", len(index.Manifests))
	}

	if _, present := index.Manifests[0]["artifactType"]; present {
		t.Errorf("artifactType is present on an index that declared none: %v", index.Manifests[0])
	}
}

// TestReferrersEmptyIsAnArray covers the most common answer the endpoint gives.
// A nil slice would marshal as null, and a client that has to defend against null
// for the ordinary case will not.
func TestReferrersEmptyIsAnArray(t *testing.T) {
	server := newServer(t)

	// Something has to be in the repository, or the answer is a 404 about the
	// repository rather than an empty list about the subject.
	raw, dgst := imageManifest(t, server, repo)
	if resp := putManifest(t, server, repo, dgst, imageManifestType, raw); resp.StatusCode != http.StatusCreated {
		t.Fatalf("push: status = %d, want 201", resp.StatusCode)
	}

	path := fmt.Sprintf("/v2/%s/referrers/%s", repo, dgst)

	resp := do(t, server, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, errorCode(t, resp))
	}

	var index struct {
		Manifests json.RawMessage `json:"manifests"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&index); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if string(index.Manifests) != "[]" {
		t.Errorf("manifests = %s, want []", index.Manifests)
	}
}

// TestReferrersSubjectNeedNotExist is the ordering real signing tools use: the
// signature is pushed against a digest the registry may never hold.
func TestReferrersSubjectNeedNotExist(t *testing.T) {
	server := newServer(t)

	subject := digest.FromString("an image this registry has never seen")
	signature := referrer(t, server, repo, subject, "application/vnd.example.signature.v1+json", nil)

	index, _ := getReferrers(t, server, repo, subject, "")

	if len(index.Manifests) != 1 || index.Manifests[0].Digest != signature {
		t.Errorf("got %v, want just %s", index.Manifests, signature)
	}
}

// TestReferrersUnknownRepositoryIsNotFound pins the one 404 this endpoint produces,
// and it is a judgement call: the spec both forbids 404 here and lists it as a
// failure code for end-12a.
//
// The reason to draw the line at the repository is that a mistyped name would
// otherwise answer "nothing refers to this", which a verification tool cannot
// distinguish from "this image is unsigned".
func TestReferrersUnknownRepositoryIsNotFound(t *testing.T) {
	server := newServer(t)

	path := fmt.Sprintf("/v2/acme/nothing/referrers/%s", digest.FromString("x"))

	resp := do(t, server, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	// NAME_UNKNOWN, not MANIFEST_UNKNOWN: what is missing is the namespace, not a
	// document in it.
	if got := errorCode(t, resp); got != "NAME_UNKNOWN" {
		t.Errorf("code = %q, want NAME_UNKNOWN", got)
	}
}

// TestReferrersScopedToRepository checks that a referrer does not leak across
// repositories even though both are describing the same subject digest and the
// bytes are in one shared content store.
func TestReferrersScopedToRepository(t *testing.T) {
	server := newServer(t)

	subject := digest.FromString("a subject two repositories both describe")

	const (
		mine   = "acme/widgets"
		theirs = "acme/gadgets"
	)

	here := referrer(t, server, mine, subject, "application/vnd.example.signature.v1+json", nil)
	there := referrer(t, server, theirs, subject, "application/vnd.example.signature.v1+json",
		map[string]string{"org.example.owner": "them"})

	index, _ := getReferrers(t, server, mine, subject, "")

	found := digestsOf(index)

	if _, ok := found[here]; !ok {
		t.Errorf("own referrer %s not listed", here)
	}

	if _, ok := found[there]; ok {
		t.Errorf("referrer %s from %s leaked into %s", there, theirs, mine)
	}
}

// TestReferrersRejectsTag pins that a tag is not a subject reference.
//
// A referrer records the digest it describes, so resolving a tag here would answer
// about whatever the tag points at now while the referrers were filed against what
// it pointed at then -- signatures would appear to vanish when a tag moved.
func TestReferrersRejectsTag(t *testing.T) {
	server := newServer(t)

	resp := do(t, server, http.MethodGet, "/v2/"+repo+"/referrers/latest", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	if got := errorCode(t, resp); got != "DIGEST_INVALID" {
		t.Errorf("code = %q, want DIGEST_INVALID", got)
	}
}

func TestReferrersRejectsUnsupportedAlgorithm(t *testing.T) {
	server := newServer(t)

	path := "/v2/" + repo + "/referrers/md5:d41d8cd98f00b204e9800998ecf8427e"

	resp := do(t, server, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	// A named algorithm this build does not implement is a "no", not a "that is
	// gibberish", and the codes for those differ.
	if got := errorCode(t, resp); got != "UNSUPPORTED" {
		t.Errorf("code = %q, want UNSUPPORTED", got)
	}
}

func TestReferrersRejectsWrites(t *testing.T) {
	server := newServer(t)

	path := fmt.Sprintf("/v2/%s/referrers/%s", repo, digest.FromString("x"))

	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			resp := do(t, server, method, path, nil, nil)
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s status = %d, want 405", method, resp.StatusCode)
			}
		})
	}
}

// TestReferrersFollowManifestDeletion checks that the listing is derived from the
// manifests present rather than from a separate record that could outlive them.
func TestReferrersFollowManifestDeletion(t *testing.T) {
	server := newServer(t)

	subject := digest.FromString("a subject")

	const artifactType = "application/vnd.example.signature.v1+json"

	first := referrer(t, server, repo, subject, artifactType, map[string]string{"n": "1"})
	second := referrer(t, server, repo, subject, artifactType, map[string]string{"n": "2"})

	path := fmt.Sprintf("/v2/%s/manifests/%s", repo, first)

	resp := do(t, server, http.MethodDelete, path, nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete status = %d, want 202: %s", resp.StatusCode, errorCode(t, resp))
	}

	index, _ := getReferrers(t, server, repo, subject, "")

	found := digestsOf(index)

	if _, ok := found[first]; ok {
		t.Errorf("deleted referrer %s is still listed", first)
	}

	if _, ok := found[second]; !ok {
		t.Errorf("surviving referrer %s is missing", second)
	}
}

// TestReferrersRejectInvalidSubjectDigest is about the push, not the listing: a
// subject that will not parse would be stored as a key no query could produce, so
// the referrer would be silently unfindable by the only endpoint that looks for it.
func TestReferrersRejectInvalidSubjectDigest(t *testing.T) {
	server := newServer(t)

	config := descriptorFor(t, server, repo, emptyConfigType, []byte("{}"))

	raw, dgst := encode(t, ocispec.Manifest{
		MediaType: imageManifestType,
		Config:    config,
		Layers:    []ocispec.Descriptor{},
		Subject:   &ocispec.Descriptor{MediaType: imageManifestType, Digest: "not-a-digest", Size: 99},
	})

	resp := putManifest(t, server, repo, dgst, imageManifestType, raw)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, errorCode(t, resp))
	}

	if got := errorCode(t, resp); got != "MANIFEST_INVALID" {
		t.Errorf("code = %q, want MANIFEST_INVALID", got)
	}
}

// TestReferrersAnnotationsSurviveTheDatabase is narrow on purpose: annotations are
// the one part of a descriptor that is not a scalar, and they take a JSON round trip
// through a text column on the way out.
func TestReferrersAnnotationsSurviveTheDatabase(t *testing.T) {
	server := newServer(t)

	subject := digest.FromString("a subject")

	annotations := map[string]string{
		"org.opencontainers.image.created": "2026-01-01T00:00:00Z",
		"org.example.quoted":               `he said "hello"`,
		"org.example.unicode":              "café ✓",
		"org.example.empty":                "",
	}

	referrer(t, server, repo, subject, "application/vnd.example.signature.v1+json", annotations)

	index, _ := getReferrers(t, server, repo, subject, "")

	if len(index.Manifests) != 1 {
		t.Fatalf("got %d referrers, want 1", len(index.Manifests))
	}

	got := index.Manifests[0].Annotations

	if len(got) != len(annotations) {
		t.Fatalf("got %d annotations, want %d: %v", len(got), len(annotations), got)
	}

	for k, want := range annotations {
		if got[k] != want {
			t.Errorf("annotation %q = %q, want %q", k, got[k], want)
		}
	}
}

// TestRepositoryNamedReferrers checks the routing rule, since a repository name may
// legally end in a component called "referrers".
func TestRepositoryNamedReferrers(t *testing.T) {
	server := newServer(t)

	const name = "acme/referrers"

	subject := digest.FromString("a subject")
	signature := referrer(t, server, name, subject, "application/vnd.example.signature.v1+json", nil)

	index, _ := getReferrers(t, server, name, subject, "")

	if len(index.Manifests) != 1 || index.Manifests[0].Digest != signature {
		t.Errorf("got %v, want just %s", index.Manifests, signature)
	}
}
