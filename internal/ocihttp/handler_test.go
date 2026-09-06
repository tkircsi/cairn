package ocihttp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/tkircsi/cairn/internal/blobstore"
	"github.com/tkircsi/cairn/internal/metastore/sqlite"
	"github.com/tkircsi/cairn/internal/ocihttp"
	"github.com/tkircsi/cairn/internal/registry"
)

const repo = "acme/widgets"

const (
	signatureType = "application/vnd.dev.cosign.artifact.sig.v1+json"
	sbomType      = "application/vnd.example.sbom.v1+json"
)

func newTestServer(t *testing.T) (*httptest.Server, *registry.Registry) {
	t.Helper()

	dir := t.TempDir()

	blobs, err := blobstore.NewFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	meta, err := sqlite.Open(context.Background(), filepath.Join(dir, "cairn.db"))
	if err != nil {
		t.Fatalf("metastore: %v", err)
	}

	t.Cleanup(func() { meta.Close() })

	reg := registry.New(blobs, meta)
	server := httptest.NewServer(ocihttp.NewHandler(reg))
	t.Cleanup(server.Close)

	return server, reg
}

// manifestJSON builds an image manifest, optionally attached to a subject.
func manifestJSON(t *testing.T, artifactType string, subject digest.Digest, annotations map[string]string) []byte {
	t.Helper()

	manifest := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeEmptyJSON,
			Digest:    ocispec.DescriptorEmptyJSON.Digest,
			Size:      ocispec.DescriptorEmptyJSON.Size,
		},
		Layers:      []ocispec.Descriptor{},
		Annotations: annotations,
	}

	if subject != "" {
		manifest.Subject = &ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    subject,
			Size:      1,
		}
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	return data
}

func getIndex(t *testing.T, server *httptest.Server, path string) (*http.Response, ocispec.Index) {
	t.Helper()

	resp, err := server.Client().Get(server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}

	defer resp.Body.Close()

	var index ocispec.Index

	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&index); err != nil {
			t.Fatalf("decode index: %v", err)
		}
	}

	return resp, index
}

// TestReferrers covers the behaviour end-12a and end-12b actually require, which
// is more than "returns a list".
func TestReferrers(t *testing.T) {
	server, reg := newTestServer(t)
	ctx := context.Background()

	subject, _, err := reg.PutManifest(ctx, repo, manifestJSON(t, "application/vnd.example.thing", "", nil))
	if err != nil {
		t.Fatalf("push subject: %v", err)
	}

	// Two signatures and one SBOM, so filtering has something to discriminate.
	for _, spec := range []struct {
		artifactType string
		annotations  map[string]string
	}{
		{signatureType, map[string]string{"org.example.signer": "alice"}},
		{signatureType, map[string]string{"org.example.signer": "bob"}},
		{sbomType, map[string]string{"org.example.format": "spdx"}},
	} {
		if _, _, err := reg.PutManifest(ctx, repo, manifestJSON(t, spec.artifactType, subject, spec.annotations)); err != nil {
			t.Fatalf("push referrer: %v", err)
		}
	}

	base := "/v2/" + repo + "/referrers/" + subject.String()

	t.Run("lists every referrer", func(t *testing.T) {
		resp, index := getIndex(t, server, base)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		// The spec fixes the Content-Type for this endpoint.
		if got := resp.Header.Get("Content-Type"); got != ocispec.MediaTypeImageIndex {
			t.Errorf("Content-Type = %q, want %q", got, ocispec.MediaTypeImageIndex)
		}

		if len(index.Manifests) != 3 {
			t.Fatalf("got %d referrers, want 3", len(index.Manifests))
		}

		if index.SchemaVersion != 2 {
			t.Errorf("schemaVersion = %d, want 2", index.SchemaVersion)
		}

		// Annotations must survive to the descriptor; clients rely on them to
		// pick a referrer without fetching each one.
		for _, desc := range index.Manifests {
			if len(desc.Annotations) == 0 {
				t.Errorf("descriptor %s lost its annotations", desc.Digest)
			}
		}

		// A filter was not requested, so the header must be absent.
		if got := resp.Header.Get("OCI-Filters-Applied"); got != "" {
			t.Errorf("OCI-Filters-Applied = %q, want empty", got)
		}
	})

	t.Run("filters by artifactType and says so", func(t *testing.T) {
		// Escaping matters: media types contain "+", which decodes as a space in
		// a query string if it is sent raw.
		resp, index := getIndex(t, server, base+"?artifactType="+url.QueryEscape(sbomType))

		if len(index.Manifests) != 1 {
			t.Fatalf("got %d referrers, want 1", len(index.Manifests))
		}

		if got := index.Manifests[0].ArtifactType; got != sbomType {
			t.Errorf("artifactType = %q, want %q", got, sbomType)
		}

		// Without this header a client cannot tell a filtered result from a
		// complete one.
		if got := resp.Header.Get("OCI-Filters-Applied"); got != "artifactType" {
			t.Errorf("OCI-Filters-Applied = %q, want artifactType", got)
		}
	})

	t.Run("paginates with a Link header", func(t *testing.T) {
		resp, index := getIndex(t, server, base+"?n=2")

		if len(index.Manifests) != 2 {
			t.Fatalf("got %d referrers, want 2", len(index.Manifests))
		}

		if resp.Header.Get("Link") == "" {
			t.Error("truncated page must carry a Link header")
		}
	})

	t.Run("unknown subject returns an empty index, not 404", func(t *testing.T) {
		// A 404 here is what sends clients to the fallback tag scheme, so it is
		// the one answer this endpoint must never give.
		absent := digest.FromString("nothing refers to this")

		resp, index := getIndex(t, server, "/v2/"+repo+"/referrers/"+absent.String())

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		if len(index.Manifests) != 0 {
			t.Errorf("got %d referrers, want 0", len(index.Manifests))
		}
	})

	t.Run("invalid digest returns 400", func(t *testing.T) {
		resp, err := server.Client().Get(server.URL + "/v2/" + repo + "/referrers/not-a-digest")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}

		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}

		var body struct {
			Errors []struct {
				Code string `json:"code"`
			} `json:"errors"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode error body: %v", err)
		}

		if len(body.Errors) != 1 || body.Errors[0].Code != "DIGEST_INVALID" {
			t.Errorf("error body = %+v, want one DIGEST_INVALID", body.Errors)
		}
	})
}

// TestEmptyIndexMarshalsAsArray guards the JSON shape: "manifests": null is not a
// valid image index, and clients differ in how badly they take it.
func TestEmptyIndexMarshalsAsArray(t *testing.T) {
	server, _ := newTestServer(t)

	absent := digest.FromString("empty")

	resp, err := server.Client().Get(server.URL + "/v2/" + repo + "/referrers/" + absent.String())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	defer resp.Body.Close()

	var raw map[string]json.RawMessage

	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got := string(raw["manifests"]); got != "[]" {
		t.Errorf("manifests = %s, want []", got)
	}
}

// TestArtifactTypeFallback covers the spec rule that an image manifest with no
// artifactType must be reported using its config descriptor's mediaType.
func TestArtifactTypeFallback(t *testing.T) {
	server, reg := newTestServer(t)
	ctx := context.Background()

	subject, _, err := reg.PutManifest(ctx, repo, manifestJSON(t, "application/vnd.example.thing", "", nil))
	if err != nil {
		t.Fatalf("push subject: %v", err)
	}

	if _, _, err := reg.PutManifest(ctx, repo, manifestJSON(t, "", subject, map[string]string{"k": "v"})); err != nil {
		t.Fatalf("push referrer: %v", err)
	}

	_, index := getIndex(t, server, "/v2/"+repo+"/referrers/"+subject.String())

	if len(index.Manifests) != 1 {
		t.Fatalf("got %d referrers, want 1", len(index.Manifests))
	}

	if got := index.Manifests[0].ArtifactType; got != ocispec.MediaTypeEmptyJSON {
		t.Errorf("artifactType = %q, want the config mediaType %q", got, ocispec.MediaTypeEmptyJSON)
	}
}

// TestReferrersAreScopedToRepository checks that referrers do not leak across
// namespaces, which the spec requires and which a single shared index would make
// easy to get wrong.
func TestReferrersAreScopedToRepository(t *testing.T) {
	server, reg := newTestServer(t)
	ctx := context.Background()

	subject, _, err := reg.PutManifest(ctx, repo, manifestJSON(t, "application/vnd.example.thing", "", nil))
	if err != nil {
		t.Fatalf("push subject: %v", err)
	}

	// Same subject digest, different repository.
	if _, _, err := reg.PutManifest(ctx, "other/repo", manifestJSON(t, signatureType, subject, map[string]string{"k": "v"})); err != nil {
		t.Fatalf("push referrer: %v", err)
	}

	_, index := getIndex(t, server, "/v2/"+repo+"/referrers/"+subject.String())

	if len(index.Manifests) != 0 {
		t.Errorf("got %d referrers, want 0: referrers leaked across repositories", len(index.Manifests))
	}
}
