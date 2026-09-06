// Package registry composes the blob store and the metadata index into the
// operations both API surfaces call.
//
// This is the only layer that understands what a manifest means. Both the gRPC
// write API and the OCI HTTP read API go through it, so there is one place where
// digests are derived, manifests are parsed and writes are ordered -- and hence
// no way for the two surfaces to disagree about what is stored.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/tkircsi/cairn/internal/blobstore"
	"github.com/tkircsi/cairn/internal/metastore"
	"github.com/tkircsi/cairn/internal/model"
)

// ErrNotFound is returned when the requested content is absent.
var ErrNotFound = errors.New("not found")

// Registry is the service layer.
type Registry struct {
	blobs blobstore.Store
	meta  metastore.Store
	now   func() time.Time
}

// New builds a Registry over the given backends.
func New(blobs blobstore.Store, meta metastore.Store) *Registry {
	return &Registry{blobs: blobs, meta: meta, now: time.Now}
}

// PutBlob stores bytes and records their membership in a repository.
func (r *Registry) PutBlob(ctx context.Context, repository string, data []byte) (digest.Digest, error) {
	dgst, err := r.blobs.Put(data)
	if err != nil {
		return "", fmt.Errorf("store blob: %w", err)
	}

	blob := model.Blob{
		Repository: repository,
		Digest:     dgst,
		Size:       int64(len(data)),
		CreatedAt:  r.now(),
	}

	if err := r.meta.PutBlob(ctx, blob); err != nil {
		return "", fmt.Errorf("index blob: %w", err)
	}

	return dgst, nil
}

// PutManifest parses, stores and indexes a manifest, returning its digest and
// the subject it refers to, if any.
func (r *Registry) PutManifest(ctx context.Context, repository string, data []byte) (digest.Digest, digest.Digest, error) {
	parsed, err := parseManifest(data)
	if err != nil {
		return "", "", err
	}

	// Blobs first, metadata second. The reverse order can leave an index row
	// pointing at content that is not there, which reads as corruption; this
	// order can at worst leave an unreferenced blob, which is inert.
	dgst, err := r.blobs.Put(data)
	if err != nil {
		return "", "", fmt.Errorf("store manifest: %w", err)
	}

	manifest := model.Manifest{
		Repository:   repository,
		Digest:       dgst,
		MediaType:    parsed.mediaType,
		ArtifactType: parsed.artifactType,
		Subject:      parsed.subject,
		Size:         int64(len(data)),
		Annotations:  parsed.annotations,
		CreatedAt:    r.now(),
	}

	if err := r.meta.PutManifest(ctx, manifest); err != nil {
		return "", "", fmt.Errorf("index manifest: %w", err)
	}

	return dgst, parsed.subject, nil
}

// Manifest returns the raw manifest bytes and their media type.
func (r *Registry) Manifest(ctx context.Context, repository string, dgst digest.Digest) ([]byte, string, error) {
	manifest, err := r.meta.Manifest(ctx, repository, dgst)
	if err != nil {
		if errors.Is(err, metastore.ErrNotFound) {
			return nil, "", ErrNotFound
		}

		return nil, "", err
	}

	data, err := r.blobs.Get(dgst)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return nil, "", ErrNotFound
		}

		return nil, "", err
	}

	return data, manifest.MediaType, nil
}

// DeleteManifest removes a manifest from the index.
//
// The blob is left alone: it may be shared, and reclaiming it is a separate
// concern. Note what does not happen -- referrers of this manifest are not
// touched, because the spec permits a subject to be absent.
func (r *Registry) DeleteManifest(ctx context.Context, repository string, dgst digest.Digest) error {
	if err := r.meta.DeleteManifest(ctx, repository, dgst); err != nil {
		if errors.Is(err, metastore.ErrNotFound) {
			return ErrNotFound
		}

		return err
	}

	return nil
}

// ReferrersPage is one page of a referrers response.
type ReferrersPage struct {
	Index ocispec.Index
	// Next is the digest to resume after, empty when the page is the last.
	Next digest.Digest
}

// Referrers answers end-12a and end-12b.
//
// An unknown subject is not an error: the spec requires a 200 with an empty
// manifest list, and a registry that supports the API must never answer 404
// here, because a 404 is precisely the signal that sends clients back to the
// fallback tag scheme.
func (r *Registry) Referrers(
	ctx context.Context,
	repository string,
	subject digest.Digest,
	artifactType string,
	limit int,
	after digest.Digest,
) (ReferrersPage, error) {
	if limit <= 0 {
		limit = 100
	}

	// Ask for one extra row to learn whether another page exists, without a
	// second COUNT query.
	found, err := r.meta.Referrers(ctx, metastore.ReferrersQuery{
		Repository:   repository,
		Subject:      subject,
		ArtifactType: artifactType,
		Limit:        limit + 1,
		After:        after,
	})
	if err != nil {
		return ReferrersPage{}, err
	}

	var next digest.Digest

	if len(found) > limit {
		found = found[:limit]
		next = found[len(found)-1].Digest
	}

	// manifests must marshal as [] rather than null when empty, so that the
	// response is a valid image index for a subject with no referrers.
	descriptors := make([]ocispec.Descriptor, 0, len(found))
	for _, m := range found {
		descriptors = append(descriptors, m.Descriptor())
	}

	page := ReferrersPage{
		Index: ocispec.Index{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageIndex,
			Manifests: descriptors,
		},
		Next: next,
	}

	return page, nil
}

// parsedManifest is what the store needs to read out of manifest bytes.
type parsedManifest struct {
	mediaType    string
	artifactType string
	subject      digest.Digest
	annotations  map[string]string
}

// parseManifest reads the indexable fields out of a manifest.
//
// Metadata is derived from the bytes rather than accepted alongside them, which
// is what makes it impossible for the index to describe something the content
// does not say.
func parseManifest(data []byte) (parsedManifest, error) {
	// ocispec.Manifest is a structural superset of what is needed from either
	// document: an index carries mediaType, artifactType, subject and
	// annotations too, and simply leaves Config zero. Config is only consulted
	// for image manifests, so an index never reads that zero value.
	var raw ocispec.Manifest

	if err := json.Unmarshal(data, &raw); err != nil {
		return parsedManifest{}, fmt.Errorf("parse manifest: %w", err)
	}

	if raw.MediaType == "" {
		return parsedManifest{}, errors.New("manifest has no mediaType")
	}

	parsed := parsedManifest{
		mediaType:    raw.MediaType,
		artifactType: raw.ArtifactType,
		annotations:  raw.Annotations,
	}

	// The spec's fallback rule for the artifactType a referrers descriptor must
	// carry: on an image manifest an empty artifactType means the config
	// descriptor's mediaType; on an index it stays empty and is omitted.
	if parsed.artifactType == "" && raw.MediaType == ocispec.MediaTypeImageManifest {
		parsed.artifactType = raw.Config.MediaType
	}

	if raw.Subject != nil {
		if err := raw.Subject.Digest.Validate(); err != nil {
			return parsedManifest{}, fmt.Errorf("invalid subject digest: %w", err)
		}

		parsed.subject = raw.Subject.Digest
	}

	return parsed, nil
}
