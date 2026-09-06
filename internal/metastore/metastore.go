// Package metastore is the metadata index over the blob store.
//
// This is the half that a filesystem-tree registry cannot provide. A path-based
// layout gives exactly one access path per tree it maintains by hand, which is
// why such registries can answer "what digest is this tag" in one read but
// cannot answer "which manifests name this subject" at all. Here that second
// question is an indexed query.
package metastore

import (
	"context"
	"errors"

	"github.com/opencontainers/go-digest"
	"github.com/tkircsi/cairn/internal/model"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store is the metadata index.
type Store interface {
	PutManifest(ctx context.Context, m model.Manifest) error
	Manifest(ctx context.Context, repository string, dgst digest.Digest) (model.Manifest, error)
	DeleteManifest(ctx context.Context, repository string, dgst digest.Digest) error

	PutBlob(ctx context.Context, b model.Blob) error

	// Referrers returns manifests whose Subject matches the query, ordered by
	// digest so that the page can be read straight off the index.
	Referrers(ctx context.Context, q ReferrersQuery) ([]model.Manifest, error)

	Close() error
}

// ReferrersQuery selects the referrers of one subject within one repository.
type ReferrersQuery struct {
	Repository string
	Subject    digest.Digest

	// ArtifactType filters when non-empty, as in end-12b.
	ArtifactType string

	// Limit caps the page. Callers ask for Limit+1 to detect truncation.
	Limit int

	// After resumes after a digest, for the Link header the spec requires when
	// a page is truncated. Keyset pagination rather than OFFSET, so page N costs
	// the same as page 1.
	After digest.Digest
}
