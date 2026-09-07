// Package registry composes the blob store, the upload area and the metadata
// index into the operations the HTTP surface calls.
//
// This is the only layer that knows the rules: that a digest is verified before
// content is promoted, that a chunk must be contiguous, that a manifest may not
// reference content the repository lacks, and that content is readable only from
// a repository that pushed it. The handler above it is left to translate those
// outcomes into status codes.
//
// Manifest operations live in manifest.go.
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/tkircsi/cairn/internal/blobstore"
	"github.com/tkircsi/cairn/internal/metastore"
	"github.com/tkircsi/cairn/internal/model"
	"github.com/tkircsi/cairn/internal/uploadstore"
)

// Failure modes the HTTP layer has to distinguish, because the spec gives each
// one a different status code and error code.
var (
	// ErrNotFound covers a blob absent from the repository being read.
	ErrNotFound = errors.New("not found")
	// ErrUploadUnknown is a session that does not exist, or no longer does.
	ErrUploadUnknown = errors.New("upload unknown")
	// ErrDigestMismatch is a completed upload whose content does not hash to the
	// digest the client claimed.
	ErrDigestMismatch = errors.New("digest mismatch")
	// ErrNotContiguous is a chunk that does not begin where the last one ended.
	ErrNotContiguous = errors.New("chunk is not contiguous")
	// ErrTooLarge is content that exceeded the configured ceiling.
	ErrTooLarge = errors.New("content exceeds the maximum size")
	// ErrManifestInvalid is a manifest that could not be parsed, or that
	// contradicts itself or the request that carried it.
	ErrManifestInvalid = errors.New("manifest invalid")
	// ErrManifestBlobUnknown is a manifest referencing content the repository
	// does not hold. Distinct from ErrNotFound because the spec gives it its own
	// code: the manifest is the thing being rejected, but the *reason* is a
	// missing child, and a client needs to know which.
	ErrManifestBlobUnknown = errors.New("manifest references unknown content")
)

// DefaultMaxBlobSize caps a single blob.
//
// Uploads are unauthenticated writes of unbounded length in the bare spec, so
// without a ceiling one client can fill the disk. The number is a policy, not a
// spec requirement, which is why it is an option.
const DefaultMaxBlobSize int64 = 1 << 30 // 1 GiB

// Registry is the service layer.
type Registry struct {
	blobs   blobstore.Store
	uploads uploadstore.Store
	meta    metastore.Store

	maxBlobSize     int64
	maxManifestSize int64
	now             func() time.Time
	log             *slog.Logger
}

// Option adjusts a Registry.
type Option func(*Registry)

// WithMaxBlobSize sets the per-blob ceiling.
func WithMaxBlobSize(n int64) Option {
	return func(r *Registry) { r.maxBlobSize = n }
}

// WithMaxManifestSize sets the per-manifest ceiling.
//
// Separate from the blob ceiling because the constraint is different: a blob is
// streamed to disk, while a manifest is held in memory to be hashed and parsed.
func WithMaxManifestSize(n int64) Option {
	return func(r *Registry) { r.maxManifestSize = n }
}

// WithClock replaces the clock, so tests can assert on timestamps.
func WithClock(fn func() time.Time) Option {
	return func(r *Registry) { r.now = fn }
}

// WithLogger sets where domain events go.
func WithLogger(logger *slog.Logger) Option {
	return func(r *Registry) {
		if logger != nil {
			r.log = logger
		}
	}
}

// New builds a Registry over the given backends.
func New(
	blobs blobstore.Store,
	uploads uploadstore.Store,
	meta metastore.Store,
	opts ...Option,
) *Registry {
	r := &Registry{
		blobs:           blobs,
		uploads:         uploads,
		meta:            meta,
		maxBlobSize:     DefaultMaxBlobSize,
		maxManifestSize: DefaultMaxManifestSize,
		now:             time.Now,
		log:             slog.Default(),
	}

	for _, opt := range opts {
		opt(r)
	}

	return r
}

// StartUpload opens a session and returns it.
func (r *Registry) StartUpload(ctx context.Context, repository string) (model.Upload, error) {
	id, err := uploadstore.NewID()
	if err != nil {
		return model.Upload{}, err
	}

	// Staging file first, row second. The reverse order can produce a session a
	// client is told to use but that has nowhere to put bytes; this order can at
	// worst leave a zero-length file no request can name.
	if err := r.uploads.Create(id); err != nil {
		return model.Upload{}, err
	}

	now := r.now()

	upload := model.Upload{
		ID:         id,
		Repository: repository,
		Received:   0,
		StartedAt:  now,
		UpdatedAt:  now,
	}

	if err := r.meta.CreateUpload(ctx, upload); err != nil {
		// Nothing can reach the file now, so drop it rather than leave it to the
		// sweeper.
		_ = r.uploads.Discard(id)

		return model.Upload{}, err
	}

	return upload, nil
}

// UploadStatus reports how much of a session has been received.
func (r *Registry) UploadStatus(ctx context.Context, id string) (model.Upload, error) {
	upload, err := r.meta.Upload(ctx, id)
	if err != nil {
		if errors.Is(err, metastore.ErrNotFound) {
			return model.Upload{}, ErrUploadUnknown
		}

		return model.Upload{}, err
	}

	return upload, nil
}

// AppendUpload adds bytes to a session.
//
// start is the offset the caller claims the chunk begins at, or -1 when it made
// no claim, which the spec permits for a streamed upload. A claim that disagrees
// with what the session holds is refused rather than reconciled: silently
// accepting it would leave a gap or a duplication in the middle of a blob, and
// the digest check at close would then fail with nothing to point at.
func (r *Registry) AppendUpload(
	ctx context.Context,
	id string,
	start int64,
	body io.Reader,
) (model.Upload, error) {
	upload, err := r.UploadStatus(ctx, id)
	if err != nil {
		return model.Upload{}, err
	}

	if start >= 0 && start != upload.Received {
		return model.Upload{}, fmt.Errorf("%w: session holds %d bytes, chunk starts at %d",
			ErrNotContiguous, upload.Received, start)
	}

	remaining := r.maxBlobSize - upload.Received
	if remaining < 0 {
		remaining = 0
	}

	// Read one byte past the ceiling: enough to detect the overrun without
	// consuming a body that may be arbitrarily long.
	received, err := r.uploads.Append(id, io.LimitReader(body, remaining+1))
	if err != nil {
		if errors.Is(err, uploadstore.ErrNotFound) {
			return model.Upload{}, ErrUploadUnknown
		}

		return model.Upload{}, err
	}

	if received > r.maxBlobSize {
		// Logged because it is a resource-exhaustion attempt as much as a client
		// error, and the access log cannot show the size that was refused.
		r.log.WarnContext(ctx, "upload exceeded the size limit",
			slog.String("upload", id),
			slog.String("repository", upload.Repository),
			slog.Int64("received", received),
			slog.Int64("limit", r.maxBlobSize),
		)

		// The staged bytes can never become a valid blob, so the session goes
		// rather than lingering at an over-limit offset.
		_ = r.discard(ctx, id)

		return model.Upload{}, fmt.Errorf("%w: %d bytes exceeds %d",
			ErrTooLarge, received, r.maxBlobSize)
	}

	// The file's length is authoritative and the row is a cache of it, so the
	// row is set from what was actually written rather than from an increment.
	if err := r.meta.SetUploadReceived(ctx, id, received, r.now()); err != nil {
		return model.Upload{}, err
	}

	upload.Received = received

	return upload, nil
}

// CompleteUpload verifies the staged bytes against expected and, only if they
// agree, promotes them into the blob store.
//
// body may carry a final chunk, which the spec allows a close to do.
func (r *Registry) CompleteUpload(
	ctx context.Context,
	id string,
	body io.Reader,
	expected digest.Digest,
) (model.Blob, error) {
	upload, err := r.UploadStatus(ctx, id)
	if err != nil {
		return model.Blob{}, err
	}

	if body != nil {
		if upload, err = r.AppendUpload(ctx, id, -1, body); err != nil {
			return model.Blob{}, err
		}
	}

	actual, err := r.uploads.Digest(id)
	if err != nil {
		if errors.Is(err, uploadstore.ErrNotFound) {
			return model.Blob{}, ErrUploadUnknown
		}

		return model.Blob{}, err
	}

	// Verified before the promotion, not after. Checking afterwards would mean
	// either trusting the claim for a moment or deleting a blob at its true
	// digest to undo the mistake -- and those bytes may be ones another
	// repository legitimately holds.
	//
	// The session survives a mismatch: the content is whatever it is, so a client
	// that miscomputed the digest can simply close again with the right one.
	if actual != expected {
		// Both digests are recorded, because the pair is the whole diagnostic: a
		// client bug produces a stable mismatch, a truncated transfer a varying one.
		r.log.WarnContext(ctx, "upload digest mismatch",
			slog.String("upload", id),
			slog.String("repository", upload.Repository),
			slog.String("claimed", expected.String()),
			slog.String("actual", actual.String()),
			slog.Int64("received", upload.Received),
		)

		return model.Blob{}, fmt.Errorf("%w: content hashes to %s, not %s",
			ErrDigestMismatch, actual, expected)
	}

	staged, err := r.uploads.Open(id)
	if err != nil {
		return model.Blob{}, err
	}

	dgst, size, err := r.blobs.Put(staged)

	closeErr := staged.Close()

	if err != nil {
		return model.Blob{}, err
	}

	if closeErr != nil {
		return model.Blob{}, fmt.Errorf("close staged upload: %w", closeErr)
	}

	blob := model.Blob{
		Repository: upload.Repository,
		Digest:     dgst,
		Size:       size,
		CreatedAt:  r.now(),
	}

	if err := r.meta.PutBlob(ctx, blob); err != nil {
		return model.Blob{}, err
	}

	r.log.InfoContext(ctx, "blob committed",
		slog.String("repository", blob.Repository),
		slog.String("digest", blob.Digest.String()),
		slog.Int64("size", blob.Size),
	)

	// The session has served its purpose. A failure to clean up is not the
	// client's problem -- the blob is committed and readable -- so it only costs
	// a stale row for the sweeper.
	_ = r.discard(ctx, id)

	return blob, nil
}

// CancelUpload abandons a session and its staged bytes.
func (r *Registry) CancelUpload(ctx context.Context, id string) error {
	if _, err := r.UploadStatus(ctx, id); err != nil {
		return err
	}

	return r.discard(ctx, id)
}

func (r *Registry) discard(ctx context.Context, id string) error {
	if err := r.uploads.Discard(id); err != nil && !errors.Is(err, uploadstore.ErrNotFound) {
		return err
	}

	if err := r.meta.DeleteUpload(ctx, id); err != nil && !errors.Is(err, metastore.ErrNotFound) {
		return err
	}

	return nil
}

// MountBlob makes a blob readable from another repository without transferring
// it.
//
// This is the endpoint that justifies the membership table: the bytes are
// already on disk under their digest, so granting a second repository access to
// them is the insertion of one row. from may be empty, in which case any
// repository holding the blob will do.
func (r *Registry) MountBlob(
	ctx context.Context,
	target, from string,
	dgst digest.Digest,
) (model.Blob, error) {
	var (
		source model.Blob
		err    error
	)

	if from != "" {
		source, err = r.meta.Blob(ctx, from, dgst)
	} else {
		source, err = r.meta.AnyBlob(ctx, dgst)
	}

	if err != nil {
		if errors.Is(err, metastore.ErrNotFound) {
			return model.Blob{}, ErrNotFound
		}

		return model.Blob{}, err
	}

	// The index says the blob exists; confirm the bytes agree before handing the
	// client a location it can read, so a mount cannot manufacture a dangling
	// reference out of a stale row.
	if _, err := r.blobs.Stat(dgst); err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return model.Blob{}, ErrNotFound
		}

		return model.Blob{}, err
	}

	blob := model.Blob{
		Repository: target,
		Digest:     dgst,
		Size:       source.Size,
		CreatedAt:  r.now(),
	}

	if err := r.meta.PutBlob(ctx, blob); err != nil {
		return model.Blob{}, err
	}

	// Worth a record of its own: this is the one way a blob becomes readable from
	// a repository that never uploaded it, so the grant should be auditable.
	r.log.InfoContext(ctx, "blob mounted",
		slog.String("repository", blob.Repository),
		slog.String("from", source.Repository),
		slog.String("digest", blob.Digest.String()),
	)

	return blob, nil
}

// StatBlob reports a blob's size, or ErrNotFound if this repository cannot read
// it.
func (r *Registry) StatBlob(
	ctx context.Context,
	repository string,
	dgst digest.Digest,
) (model.Blob, error) {
	blob, err := r.meta.Blob(ctx, repository, dgst)
	if err != nil {
		if errors.Is(err, metastore.ErrNotFound) {
			return model.Blob{}, ErrNotFound
		}

		return model.Blob{}, err
	}

	return blob, nil
}

// OpenBlob returns a handle to a blob's bytes.
//
// Membership is checked first and the content second. That order is what scopes
// reads: a caller asking a repository for a digest it never pushed gets
// ErrNotFound even though the bytes are sitting in the shared store.
func (r *Registry) OpenBlob(
	ctx context.Context,
	repository string,
	dgst digest.Digest,
) (io.ReadSeekCloser, model.Blob, error) {
	blob, err := r.StatBlob(ctx, repository, dgst)
	if err != nil {
		return nil, model.Blob{}, err
	}

	content, err := r.blobs.Open(dgst)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return nil, model.Blob{}, ErrNotFound
		}

		return nil, model.Blob{}, err
	}

	return content, blob, nil
}

// DeleteBlob drops a repository's access to a blob.
//
// The bytes are left alone. Another repository may hold the same digest, and
// deciding that this was the last reference needs a sweep this store does not
// perform.
func (r *Registry) DeleteBlob(ctx context.Context, repository string, dgst digest.Digest) error {
	if err := r.meta.DeleteBlob(ctx, repository, dgst); err != nil {
		if errors.Is(err, metastore.ErrNotFound) {
			return ErrNotFound
		}

		return err
	}

	return nil
}
