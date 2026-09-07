package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/tkircsi/cairn/internal/metastore/sqlite"
	"github.com/tkircsi/cairn/internal/model"
)

func open(t *testing.T) *sqlite.Store {
	t.Helper()

	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "cairn.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() { store.Close() })

	return store
}

// TestIsReferencedSpansBothTables is the test that earns the method's existence.
//
// Content is shared and reference rows are typed, so liveness is a question about
// *every* table that points at bytes. The case that matters is the third one: a
// digest with no blob row at all, still needed by a manifest row. A sweeper
// consulting only blob membership would delete those bytes and leave a manifest
// whose HEAD succeeds and whose GET returns 404.
func TestIsReferencedSpansBothTables(t *testing.T) {
	ctx := context.Background()
	store := open(t)

	const repo = "acme/widgets"

	var (
		layer    = digest.FromString("a layer")
		manifest = digest.FromString("a manifest")
		orphan   = digest.FromString("bytes nothing points at")
	)

	if err := store.PutBlob(ctx, model.Blob{
		Repository: repo,
		Digest:     layer,
		Size:       7,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	// Note there is deliberately no blobs row for this digest, which is how
	// PutManifest behaves: manifests share the content store but are not members of
	// the repository's blob set.
	if err := store.PutManifest(ctx, model.Manifest{
		Repository: repo,
		Digest:     manifest,
		MediaType:  "application/vnd.oci.image.manifest.v1+json",
		Size:       9,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}

	for _, tc := range []struct {
		name string
		dgst digest.Digest
		want bool
	}{
		{"referenced by a blob row", layer, true},
		{"referenced by a manifest row only", manifest, true},
		{"referenced by nothing", orphan, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.IsReferenced(ctx, tc.dgst)
			if err != nil {
				t.Fatalf("IsReferenced: %v", err)
			}

			if got != tc.want {
				t.Errorf("IsReferenced(%s) = %v, want %v", tc.dgst, got, tc.want)
			}
		})
	}
}

// TestIsReferencedAcrossRepositories covers the other reason the question cannot
// be answered per repository: the same bytes may be held by several, and the last
// row to go is what makes them collectable.
func TestIsReferencedAcrossRepositories(t *testing.T) {
	ctx := context.Background()
	store := open(t)

	shared := digest.FromString("bytes two repositories hold")

	for _, repo := range []string{"acme/widgets", "acme/gadgets"} {
		if err := store.PutBlob(ctx, model.Blob{
			Repository: repo,
			Digest:     shared,
			Size:       11,
			CreatedAt:  time.Now(),
		}); err != nil {
			t.Fatalf("PutBlob(%s): %v", repo, err)
		}
	}

	if err := store.DeleteBlob(ctx, "acme/widgets", shared); err != nil {
		t.Fatalf("DeleteBlob: %v", err)
	}

	referenced, err := store.IsReferenced(ctx, shared)
	if err != nil {
		t.Fatalf("IsReferenced: %v", err)
	}

	if !referenced {
		t.Error("bytes became collectable while another repository still held them")
	}

	if err := store.DeleteBlob(ctx, "acme/gadgets", shared); err != nil {
		t.Fatalf("DeleteBlob: %v", err)
	}

	referenced, err = store.IsReferenced(ctx, shared)
	if err != nil {
		t.Fatalf("IsReferenced: %v", err)
	}

	if referenced {
		t.Error("bytes are still reported live after the last row was deleted")
	}
}

// TestIsReferencedSurvivesTheBlobRowGoing is the specific ordering that would
// corrupt a manifest: the blob and the manifest are the same bytes, and revoking
// the blob membership must not make them collectable.
func TestIsReferencedSurvivesTheBlobRowGoing(t *testing.T) {
	ctx := context.Background()
	store := open(t)

	const repo = "acme/widgets"

	shared := digest.FromString("bytes that are both a blob and a manifest")

	if err := store.PutBlob(ctx, model.Blob{
		Repository: repo, Digest: shared, Size: 3, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	if err := store.PutManifest(ctx, model.Manifest{
		Repository: repo,
		Digest:     shared,
		MediaType:  "application/vnd.oci.image.manifest.v1+json",
		Size:       3,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}

	if err := store.DeleteBlob(ctx, repo, shared); err != nil {
		t.Fatalf("DeleteBlob: %v", err)
	}

	referenced, err := store.IsReferenced(ctx, shared)
	if err != nil {
		t.Fatalf("IsReferenced: %v", err)
	}

	if !referenced {
		t.Error("deleting the blob row made a live manifest's bytes collectable")
	}
}

// TestManifestRoundTripsThroughSQL checks the columns that only exist because a
// digest cannot answer for them, including the subject pointer the referrers
// endpoint will select on.
func TestManifestRoundTripsThroughSQL(t *testing.T) {
	ctx := context.Background()
	store := open(t)

	const repo = "acme/widgets"

	want := model.Manifest{
		Repository:   repo,
		Digest:       digest.FromString("a signature"),
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		ArtifactType: "application/vnd.example.signature.v1+json",
		Subject:      digest.FromString("the thing it signs"),
		Size:         321,
		CreatedAt:    time.Now().UTC().Truncate(time.Millisecond),
	}

	if err := store.PutManifest(ctx, want); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}

	got, err := store.Manifest(ctx, repo, want.Digest)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	if got.MediaType != want.MediaType {
		t.Errorf("MediaType = %q, want %q", got.MediaType, want.MediaType)
	}

	if got.ArtifactType != want.ArtifactType {
		t.Errorf("ArtifactType = %q, want %q", got.ArtifactType, want.ArtifactType)
	}

	if got.Subject != want.Subject {
		t.Errorf("Subject = %q, want %q", got.Subject, want.Subject)
	}

	if got.Size != want.Size {
		t.Errorf("Size = %d, want %d", got.Size, want.Size)
	}

	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %s, want %s", got.CreatedAt, want.CreatedAt)
	}
}

// TestManifestWithoutASubjectStoresAnEmptyPointer pins the choice of ” over NULL:
// the Go zero value has to survive the round trip, or every read would need a
// sql.NullString and every caller a nil check.
func TestManifestWithoutASubjectStoresAnEmptyPointer(t *testing.T) {
	ctx := context.Background()
	store := open(t)

	const repo = "acme/widgets"

	dgst := digest.FromString("a manifest that stands alone")

	if err := store.PutManifest(ctx, model.Manifest{
		Repository: repo,
		Digest:     dgst,
		MediaType:  "application/vnd.oci.image.manifest.v1+json",
		Size:       5,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}

	got, err := store.Manifest(ctx, repo, dgst)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	if got.Subject != "" {
		t.Errorf("Subject = %q, want empty", got.Subject)
	}

	if got.ArtifactType != "" {
		t.Errorf("ArtifactType = %q, want empty", got.ArtifactType)
	}
}
