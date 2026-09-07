package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/tkircsi/cairn/internal/blobstore"
	"github.com/tkircsi/cairn/internal/metastore"
	"github.com/tkircsi/cairn/internal/model"
)

// DefaultMaxManifestSize caps a manifest.
//
// Manifests are read into memory to be hashed and parsed, unlike blobs which are
// streamed, so this ceiling bounds allocation rather than just disk. 4 MiB is
// what the ecosystem settled on and no legitimate manifest approaches it.
const DefaultMaxManifestSize int64 = 4 << 20

// manifestDoc is the subset of the manifest schemas this store reads.
//
// One struct covers both an image manifest and an index because the fields do
// not collide: a manifest has config and layers, an index has manifests, and
// which are present says which document arrived without having to trust
// mediaType to decide it.
type manifestDoc struct {
	MediaType    string               `json:"mediaType"`
	ArtifactType string               `json:"artifactType"`
	Config       *ocispec.Descriptor  `json:"config"`
	Layers       []ocispec.Descriptor `json:"layers"`
	Manifests    []ocispec.Descriptor `json:"manifests"`
	Subject      *ocispec.Descriptor  `json:"subject"`
}

// PutManifest verifies a manifest and records it.
//
// The digest is checked against the bytes, the document is parsed, and
// everything it references is required to be present in this repository already.
// That last check is what keeps the store from accumulating manifests that
// describe content nobody can fetch: a client is expected to push leaves before
// the node that names them, and enforcing it here means a manifest that exists
// is a manifest that resolves.
//
// mediaType is the request's Content-Type, and is only used to catch a client
// disagreeing with its own document; the value recorded comes from the bytes.
//
// expected may be empty, which is how a push by tag arrives: the client names the
// manifest by a tag instead of a digest and so makes no claim about its content.
func (r *Registry) PutManifest(
	ctx context.Context,
	repository string,
	expected digest.Digest,
	mediaType string,
	body io.Reader,
) (model.Manifest, error) {
	// One byte past the ceiling, so an over-long manifest is detected without
	// reading a body that may be arbitrarily long.
	raw, err := io.ReadAll(io.LimitReader(body, r.maxManifestSize+1))
	if err != nil {
		return model.Manifest{}, fmt.Errorf("read manifest: %w", err)
	}

	if int64(len(raw)) > r.maxManifestSize {
		r.log.WarnContext(ctx, "manifest exceeded the size limit",
			slog.String("repository", repository),
			slog.Int("size", len(raw)),
			slog.Int64("limit", r.maxManifestSize),
		)

		return model.Manifest{}, fmt.Errorf("%w: %d bytes exceeds %d",
			ErrTooLarge, len(raw), r.maxManifestSize)
	}

	// Derived from the bytes, then compared -- the same rule as a blob. A manifest
	// is the document other content is trusted through, so accepting a claimed
	// digest here would undermine every signature that points at it.
	//
	// An empty expected means the client pushed by tag and made no claim. The
	// digest is still computed from the bytes; there is simply nothing to disagree
	// with, so there is nothing to reject.
	if actual := digest.FromBytes(raw); expected != "" && actual != expected {
		r.log.WarnContext(ctx, "manifest digest mismatch",
			slog.String("repository", repository),
			slog.String("claimed", expected.String()),
			slog.String("actual", actual.String()),
		)

		return model.Manifest{}, fmt.Errorf("%w: content hashes to %s, not %s",
			ErrDigestMismatch, actual, expected)
	}

	doc, err := parseManifest(raw, mediaType)
	if err != nil {
		return model.Manifest{}, err
	}

	if err := r.checkReferences(ctx, repository, doc); err != nil {
		return model.Manifest{}, err
	}

	// Bytes first, row second, as with a blob: an index entry pointing at absent
	// content reads as corruption, while unreferenced bytes are inert.
	//
	// Note what is *not* done here: no membership row is written to blobs. The two
	// namespaces share the content store but not addressing, so this digest stays
	// a 404 on the blob endpoints.
	dgst, size, err := r.blobs.Put(bytes.NewReader(raw))
	if err != nil {
		return model.Manifest{}, err
	}

	manifest := model.Manifest{
		Repository:   repository,
		Digest:       dgst,
		MediaType:    doc.MediaType,
		ArtifactType: doc.ArtifactType,
		CreatedAt:    r.now(),
		Size:         size,
	}

	if doc.Subject != nil {
		manifest.Subject = doc.Subject.Digest
	}

	if err := r.meta.PutManifest(ctx, manifest); err != nil {
		return model.Manifest{}, err
	}

	r.log.InfoContext(ctx, "manifest committed",
		slog.String("repository", manifest.Repository),
		slog.String("digest", manifest.Digest.String()),
		slog.String("mediaType", manifest.MediaType),
		slog.String("subject", manifest.Subject.String()),
		slog.Int64("size", manifest.Size),
	)

	return manifest, nil
}

// parseManifest decodes a manifest and rejects the shapes a registry should not
// store.
func parseManifest(raw []byte, contentType string) (manifestDoc, error) {
	var doc manifestDoc

	// DisallowUnknownFields is deliberately *not* set. Manifests are extensible by
	// design -- annotations and later spec fields must survive a round trip -- and
	// the bytes are stored verbatim anyway, so parsing is only to learn about
	// them, not to re-emit them.
	if err := json.Unmarshal(raw, &doc); err != nil {
		return manifestDoc{}, fmt.Errorf("%w: not a JSON document: %v", ErrManifestInvalid, err)
	}

	declared, err := declaredMediaType(contentType)
	if err != nil {
		return manifestDoc{}, err
	}

	switch {
	// The spec makes mediaType a SHOULD, not a MUST, so a document without one is
	// not malformed. But *something* has to be recorded, because it is what a GET
	// answers with, and the request's Content-Type is exactly that something.
	case doc.MediaType == "" && declared == "":
		return manifestDoc{}, fmt.Errorf(
			"%w: neither the manifest's mediaType nor a Content-Type header says what this is",
			ErrManifestInvalid)

	case doc.MediaType == "":
		doc.MediaType = declared

	// A disagreement between the envelope and the document is a client bug worth
	// surfacing: the two describe the same bytes, and which one a proxy or cache
	// downstream believes is not knowable from here. The spec makes this a client
	// MUST, so refusing it is enforcing the client's own obligation.
	//
	// Compared case-insensitively because media types are, per RFC 2045.
	case declared != "" && !strings.EqualFold(declared, doc.MediaType):
		return manifestDoc{}, fmt.Errorf(
			"%w: Content-Type %q disagrees with the manifest's mediaType %q",
			ErrManifestInvalid, declared, doc.MediaType)
	}

	if doc.Config == nil && len(doc.Layers) == 0 && len(doc.Manifests) == 0 {
		return manifestDoc{}, fmt.Errorf(
			"%w: document has neither config, layers nor manifests", ErrManifestInvalid)
	}

	return doc, nil
}

// declaredMediaType reads the media type out of a Content-Type header, discarding
// any parameters.
//
// The spec says a registry SHOULD ignore parameters, so a conformant client
// sending "application/vnd.oci.image.manifest.v1+json; charset=utf-8" must not be
// refused for it -- which a plain string comparison against mediaType would do.
//
// A header that will not parse at all is refused rather than ignored. Being
// lenient about a malformed header while refusing a merely mismatched one would
// be incoherent, and both mean the same thing: the client is not describing its
// own bytes correctly.
func declaredMediaType(header string) (string, error) {
	if header == "" {
		return "", nil
	}

	parsed, _, err := mime.ParseMediaType(header)
	if err != nil {
		return "", fmt.Errorf("%w: malformed Content-Type %q: %v", ErrManifestInvalid, header, err)
	}

	return parsed, nil
}

// checkReferences requires everything a manifest names to be present in the same
// repository.
//
// Blobs and manifests are checked against their own namespace, chosen by which
// field the descriptor came from rather than by its mediaType -- a client is free
// to be wrong about a media type, but not about where it put the content.
//
// The subject is exempt, and that exemption is the spec's: a signature or SBOM
// may be pushed before, or entirely without, the thing it describes. Requiring
// it would break the ordering real signing tools use.
func (r *Registry) checkReferences(ctx context.Context, repository string, doc manifestDoc) error {
	if doc.Config != nil && doc.Config.Digest != "" {
		if err := r.requireBlob(ctx, repository, doc.Config.Digest, "config"); err != nil {
			return err
		}
	}

	for i, layer := range doc.Layers {
		if err := r.requireBlob(ctx, repository, layer.Digest, fmt.Sprintf("layer %d", i)); err != nil {
			return err
		}
	}

	// No "exists elsewhere" hint for index children, because there would be nothing
	// to suggest: the spec has no manifest equivalent of end-11, so a manifest in
	// another repository has to be pushed again rather than mounted.
	for i, child := range doc.Manifests {
		if _, err := r.meta.Manifest(ctx, repository, child.Digest); err != nil {
			if errors.Is(err, metastore.ErrNotFound) {
				return fmt.Errorf("%w: manifest %d (%s) is not in %s",
					ErrManifestBlobUnknown, i, child.Digest, repository)
			}

			return err
		}
	}

	return nil
}

func (r *Registry) requireBlob(
	ctx context.Context,
	repository string,
	dgst digest.Digest,
	what string,
) error {
	if err := dgst.Validate(); err != nil {
		return fmt.Errorf("%w: %s has an invalid digest %q", ErrManifestInvalid, what, dgst)
	}

	_, err := r.meta.Blob(ctx, repository, dgst)
	if err == nil {
		return nil
	}

	if !errors.Is(err, metastore.ErrNotFound) {
		return err
	}

	// "Nowhere" and "elsewhere" are different problems with different remedies, and
	// only the second one has a cheap fix. Probing costs a query on the failure
	// path only.
	//
	// The other repository is deliberately not named. Today there is no
	// authentication so it would leak nothing that is not already public, but the
	// message would become a disclosure the moment there is -- and naming it is not
	// needed, because a mount with no "from" resolves the source itself.
	//
	// Worth recording that even the hint is a weak existence oracle once auth
	// exists: a caller who guesses a digest learns whether the registry holds it.
	if _, elsewhere := r.meta.AnyBlob(ctx, dgst); elsewhere == nil {
		return fmt.Errorf(
			"%w: %s (%s) is not in %s, but is present elsewhere in this registry; "+
				"mount it with POST /v2/%s/blobs/uploads/?mount=%s",
			ErrManifestBlobUnknown, what, dgst, repository, repository, dgst)
	}

	return fmt.Errorf("%w: %s (%s) is not in %s",
		ErrManifestBlobUnknown, what, dgst, repository)
}

// Manifest returns a manifest's bytes and what is recorded about it.
//
// Membership is checked before the content, the same ordering that scopes blob
// reads: a repository cannot read a manifest it never pushed, even though the
// bytes are in the shared store.
func (r *Registry) Manifest(
	ctx context.Context,
	repository string,
	dgst digest.Digest,
) ([]byte, model.Manifest, error) {
	manifest, err := r.StatManifest(ctx, repository, dgst)
	if err != nil {
		return nil, model.Manifest{}, err
	}

	// Get rather than Open: a manifest is small and bounded, so it can be verified
	// against its digest on the way out. Blobs cannot afford that, because
	// verifying means reading the whole thing before answering and would defeat
	// Range. Here the check is close to free.
	raw, err := r.blobs.Get(dgst)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return nil, model.Manifest{}, ErrNotFound
		}

		return nil, model.Manifest{}, err
	}

	return raw, manifest, nil
}

// StatManifest reports what is recorded about a manifest without reading it,
// which is all a HEAD needs.
func (r *Registry) StatManifest(
	ctx context.Context,
	repository string,
	dgst digest.Digest,
) (model.Manifest, error) {
	manifest, err := r.meta.Manifest(ctx, repository, dgst)
	if err != nil {
		if errors.Is(err, metastore.ErrNotFound) {
			return model.Manifest{}, ErrNotFound
		}

		return model.Manifest{}, err
	}

	return manifest, nil
}

// DeleteManifest drops a repository's record of a manifest, leaving the bytes.
func (r *Registry) DeleteManifest(
	ctx context.Context,
	repository string,
	dgst digest.Digest,
) error {
	if err := r.meta.DeleteManifest(ctx, repository, dgst); err != nil {
		if errors.Is(err, metastore.ErrNotFound) {
			return ErrNotFound
		}

		return err
	}

	r.log.InfoContext(ctx, "manifest deleted",
		slog.String("repository", repository),
		slog.String("digest", dgst.String()),
	)

	return nil
}
