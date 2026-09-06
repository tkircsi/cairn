// Package metastore is the index over the blob store.
//
// It holds no bytes. Its job is to answer the questions a content-addressed
// store cannot: which repository may read a given blob, and what state an
// in-progress upload is in.
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

	CreateUpload(ctx context.Context, u model.Upload) error
	Upload(ctx context.Context, id string) (model.Upload, error)
	SetUploadReceived(ctx context.Context, id string, received int64, at time.Time) error
	DeleteUpload(ctx context.Context, id string) error

	Close() error
}
