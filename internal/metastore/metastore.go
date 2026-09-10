// Package metastore is the index over the blob store.
//
// It holds no bytes. Its job is to answer the questions a content-addressed
// store cannot: which repository may read a given blob, what a manifest claims
// to be, what name currently points at which manifest, what refers to a given
// manifest, and what state an in-progress upload is in.
//
// The referrers question is the one that could not be answered any other way. The
// rest are conveniences a walk of the store could reconstruct slowly; "what points
// at this" is not derivable from the bytes being asked about at all, and its answer
// changes without them changing.
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

// ErrBusy is the store refusing a write because another writer holds the lock.
//
// It is separated from every other write failure because it is the only one that is
// not a failure of the request. Nothing was wrong with what the caller asked for, and
// asking again is very likely to work -- which makes it the one storage error a client
// can act on, and the one that must not be reported as an internal fault.
//
// The distinction only pays off if it survives the trip out. An implementation MUST
// wrap this rather than return it bare, so the driver's own message and result code
// still reach the log; the caller decides what to do from errors.Is and the operator
// diagnoses from the text.
var ErrBusy = errors.New("store is busy")

// Store indexes blob membership, manifests, tags, referrers and upload sessions.
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
	// DeleteManifest removes a manifest along with every tag in the repository
	// pointing at it. The cascade is part of the contract, not an implementation
	// detail: the spec requires a tag to stop resolving once its manifest is
	// deleted, and an implementation that left the rows would advertise tags that
	// cannot be fetched.
	DeleteManifest(ctx context.Context, repository string, dgst digest.Digest) error
	// Referrers lists the manifests in a repository that name subject, newest
	// first, optionally narrowed to one artifact type. An empty artifactType means
	// no filter, which is unambiguous because the empty string is not a legal
	// artifact type.
	//
	// The subject need not exist, and an empty result is not an error. Both follow
	// from what the question means: "what points at this" is answerable without the
	// target being present, and a registry that 404'd instead would be telling a
	// client that no signatures exist in the same way it reports a broken URL.
	//
	// Implementations MUST return the effective artifact type rather than what the
	// document literally said, and MUST return the manifest's annotations. Those
	// are what a client uses to choose which referrer to fetch, so a response
	// without them forces it to fetch all of them.
	Referrers(
		ctx context.Context,
		repository string,
		subject digest.Digest,
		artifactType string,
	) ([]model.Manifest, error)

	// PutTag points a tag at a manifest, creating it or moving it if it exists.
	// Moving is expected traffic rather than a conflict, which is how a release
	// name follows a new build.
	PutTag(ctx context.Context, t model.Tag) error
	Tag(ctx context.Context, repository, name string) (model.Tag, error)
	// Tags lists tag names in ASCIIbetical order, beginning strictly after the
	// cursor, which need not name a tag that exists.
	//
	// A negative limit means no limit; zero means no rows. Those are deliberately
	// different, because the API distinguishes them: a request with no n asks for
	// everything, and a request with n=0 asks for nothing and must not be quietly
	// answered with everything.
	Tags(ctx context.Context, repository, after string, limit int) ([]string, error)
	DeleteTag(ctx context.Context, repository, name string) error

	// RepositoryExists reports whether anything is filed under a name.
	//
	// Repositories are not created and there is no table of them; a repository is
	// the scope a push names. But the spec still distinguishes an empty repository
	// from one that does not exist, so the fact has to be derived from the other
	// tables.
	RepositoryExists(ctx context.Context, repository string) (bool, error)

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
