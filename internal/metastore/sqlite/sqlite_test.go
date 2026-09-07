package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/tkircsi/cairn/internal/metastore"
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

// putTagged is a manifest plus a tag pointing at it, which is the only way a tag
// can legitimately come into being.
func putTagged(t *testing.T, store *sqlite.Store, repo, tag string, dgst digest.Digest) {
	t.Helper()

	ctx := context.Background()

	if err := store.PutManifest(ctx, model.Manifest{
		Repository: repo,
		Digest:     dgst,
		MediaType:  "application/vnd.oci.image.manifest.v1+json",
		Size:       1,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}

	if err := store.PutTag(ctx, model.Tag{
		Repository: repo,
		Name:       tag,
		Digest:     dgst,
		UpdatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("PutTag: %v", err)
	}
}

// TestTagsLimitConventions pins the two ends of the limit contract, which the API
// depends on being distinguishable: a request with no n asks for everything and a
// request with n=0 asks for nothing.
func TestTagsLimitConventions(t *testing.T) {
	ctx := context.Background()
	store := open(t)

	const repo = "acme/widgets"

	for _, tag := range []string{"a", "b", "c"} {
		putTagged(t, store, repo, tag, digest.FromString(tag))
	}

	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{"negative means unlimited", -1, 3},
		{"zero means none", 0, 0},
		{"positive caps", 2, 2},
		{"beyond the end", 10, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names, err := store.Tags(ctx, repo, "", tc.limit)
			if err != nil {
				t.Fatalf("Tags: %v", err)
			}

			if len(names) != tc.want {
				t.Errorf("got %d tags (%v), want %d", len(names), names, tc.want)
			}
		})
	}
}

// TestTagsOrderIsByteOrder pins the collation the pagination cursor depends on. If
// this table were declared COLLATE NOCASE, the order would differ from the one
// clients compute with sort.Strings and a paginated walk would skip tags.
func TestTagsOrderIsByteOrder(t *testing.T) {
	ctx := context.Background()
	store := open(t)

	const repo = "acme/widgets"

	pushed := []string{"b", "B", "a", "A", "_", "z1", "z10", "z2"}
	for _, tag := range pushed {
		putTagged(t, store, repo, tag, digest.FromString(tag))
	}

	got, err := store.Tags(ctx, repo, "", -1)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}

	want := append([]string(nil), pushed...)
	sort.Strings(want)

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// TestPutTagMoves covers the upsert: repointing a name is normal traffic, so it
// must not be a conflict, and it must leave one row rather than two.
func TestPutTagMoves(t *testing.T) {
	ctx := context.Background()
	store := open(t)

	const repo = "acme/widgets"

	first := digest.FromString("the first build")
	second := digest.FromString("the second build")

	putTagged(t, store, repo, "latest", first)
	putTagged(t, store, repo, "latest", second)

	tag, err := store.Tag(ctx, repo, "latest")
	if err != nil {
		t.Fatalf("Tag: %v", err)
	}

	if tag.Digest != second {
		t.Errorf("latest = %s, want %s", tag.Digest, second)
	}

	names, err := store.Tags(ctx, repo, "", -1)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}

	if len(names) != 1 {
		t.Errorf("moving a tag produced %d rows (%v), want 1", len(names), names)
	}
}

// TestDeleteManifestCascade covers the transaction. Two tags point at the manifest
// and a third points elsewhere; only the first two may go.
func TestDeleteManifestCascade(t *testing.T) {
	ctx := context.Background()
	store := open(t)

	const repo = "acme/widgets"

	doomed := digest.FromString("the manifest being deleted")
	survivor := digest.FromString("an unrelated manifest")

	putTagged(t, store, repo, "v1", doomed)
	putTagged(t, store, repo, "latest", doomed)
	putTagged(t, store, repo, "stable", survivor)

	// A tag of the same name in another repository must not be touched: the cascade
	// is scoped, and the shared content store makes it easy to get this wrong.
	putTagged(t, store, "acme/gadgets", "v1", doomed)

	if err := store.DeleteManifest(ctx, repo, doomed); err != nil {
		t.Fatalf("DeleteManifest: %v", err)
	}

	names, err := store.Tags(ctx, repo, "", -1)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}

	if want := []string{"stable"}; len(names) != 1 || names[0] != want[0] {
		t.Errorf("tags = %v, want %v", names, want)
	}

	elsewhere, err := store.Tags(ctx, "acme/gadgets", "", -1)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}

	if len(elsewhere) != 1 {
		t.Errorf("cascade crossed a repository boundary: %v", elsewhere)
	}
}

// TestDeleteManifestRollsBackWhenAbsent covers the other half of the transaction:
// a caller told the manifest was not there must not have had side effects.
func TestDeleteManifestRollsBackWhenAbsent(t *testing.T) {
	ctx := context.Background()
	store := open(t)

	const repo = "acme/widgets"

	// A tag pointing at a digest whose manifest row is not in this repository. The
	// delete finds tags to remove and no manifest, so it must undo the tag removal.
	shared := digest.FromString("held as a manifest elsewhere only")

	putTagged(t, store, "acme/gadgets", "v1", shared)

	if err := store.PutTag(ctx, model.Tag{
		Repository: repo,
		Name:       "dangling",
		Digest:     shared,
		UpdatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("PutTag: %v", err)
	}

	if err := store.DeleteManifest(ctx, repo, shared); !errors.Is(err, metastore.ErrNotFound) {
		t.Fatalf("DeleteManifest err = %v, want ErrNotFound", err)
	}

	names, err := store.Tags(ctx, repo, "", -1)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}

	if len(names) != 1 {
		t.Errorf("tag rows were destroyed by a delete that reported not-found: %v", names)
	}
}

func TestRepositoryExists(t *testing.T) {
	ctx := context.Background()
	store := open(t)

	exists, err := store.RepositoryExists(ctx, "acme/nothing")
	if err != nil {
		t.Fatalf("RepositoryExists: %v", err)
	}

	if exists {
		t.Error("a repository nothing was ever filed under reports as existing")
	}

	// A blob alone is enough. There is no create step, so holding any one thing is
	// what existence means.
	if err := store.PutBlob(ctx, model.Blob{
		Repository: "acme/widgets",
		Digest:     digest.FromString("a layer"),
		Size:       7,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	exists, err = store.RepositoryExists(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("RepositoryExists: %v", err)
	}

	if !exists {
		t.Error("a repository holding a blob reports as absent")
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
