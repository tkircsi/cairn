// Package metastore is the index over the blob store.
//
// It holds no bytes. Its job is to answer the questions a content-addressed
// store cannot: which repository may read a given blob, what a manifest claims
// to be, and what state an in-progress upload is in.
package metastore

import (
	"context"
	"errors"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/tkircsi/cairn/internal/model"
)

// ErrNotFound is returned when a row is absent.
var ErrNotFound = errors.New("not found")

// Store indexes blob membership and upload sessions.
type Store interface {
	// PutBlob records that a repository contains a blob. It is idempotent: the
	// same bytes pushed twice is the normal case, not an error.
	PutBlob(ctx context.Context, b model.Blob) error
	// Blob reports what a specific repository holds. A blob present in some
	// other repository is ErrNotFound here, which is what keeps repositories
	// from reading each other's content out of the shared store.
	Blob(ctx context.Context, repository string, dgst digest.Digest) (model.Blob, error)
	// AnyBlob reports whether any repository holds the blob, and is what makes a
	// cross-repository mount cheap: the bytes are already on disk, so mounting is
	// only the insertion of a membership row.
	AnyBlob(ctx context.Context, dgst digest.Digest) (model.Blob, error)
	DeleteBlob(ctx context.Context, repository string, dgst digest.Digest) error

	// PutManifest records a manifest. Idempotent for the same reason PutBlob is:
	// a manifest is named by the digest of its own bytes, so a repeated push
	// carries identical content and there is nothing to change.
	PutManifest(ctx context.Context, m model.Manifest) error
	// Manifest reports what a repository holds at a digest, including the media
	// type a GET must answer with.
	Manifest(ctx context.Context, repository string, dgst digest.Digest) (model.Manifest, error)
	DeleteManifest(ctx context.Context, repository string, dgst digest.Digest) error

	CreateUpload(ctx context.Context, u model.Upload) error
	Upload(ctx context.Context, id string) (model.Upload, error)
	SetUploadReceived(ctx context.Context, id string, received int64, at time.Time) error
	DeleteUpload(ctx context.Context, id string) error

	// IsReferenced reports whether any row anywhere still needs the bytes at dgst.
	//
	// This exists for a garbage collector, which has no other way to ask. Deleting
	// a blob or a manifest removes only a row, so content accumulates, and the
	// bytes are shared: the same digest may be a blob of one repository and a
	// manifest of another. A sweeper that consulted only the blob membership rows
	// would delete every manifest's bytes while the manifest rows still pointed at
	// them, producing a manifest whose HEAD succeeds and whose GET does not.
	//
	// So the obligation is stated once, here, rather than left for whoever writes
	// the sweeper to rediscover: an implementation MUST consider every table that
	// references content.
	//
	// The answer is only true as of the moment it is asked. Content is
	// deduplicated, so a digest that is unreferenced now becomes referenced the
	// instant anyone pushes the same bytes -- a sweeper needs a grace period or a
	// maintenance window, not just this predicate.
	IsReferenced(ctx context.Context, dgst digest.Digest) (bool, error)

	Close() error
}
