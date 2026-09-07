package ocihttp

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/tkircsi/cairn/internal/registry"
)

// maxTagsPerRequest caps the ?tag= parameters on one push.
//
// The spec asks for at least 10 and allows a 414 beyond whatever the registry
// supports. A limit exists mainly so the behaviour is defined rather than
// inherited from whatever net/http happens to accept in a request line.
const maxTagsPerRequest = 64

// manifest routes end-3, end-7 and end-9, which share a URL.
//
// The reference is either a digest or a tag, and which one it is changes what the
// method means rather than just how the target is found: a DELETE of a digest
// forgets content, a DELETE of a tag forgets only a name.
func (h *Handler) manifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	dgst, err := digest.Parse(reference)
	if err == nil {
		h.manifestByDigest(w, r, name, dgst)

		return
	}

	switch {
	case registry.ValidTag(reference):
		h.manifestByTag(w, r, name, reference)

	// A named algorithm this build does not implement is a "no" rather than a
	// "that is gibberish", and go-digest tells the two apart, so there is no
	// reason to collapse them.
	case errors.Is(err, digest.ErrDigestUnsupported):
		writeError(w, http.StatusBadRequest, "UNSUPPORTED", "unsupported digest algorithm")

	default:
		writeError(w, http.StatusBadRequest, "MANIFEST_INVALID",
			"reference is neither a digest nor a valid tag")
	}
}

func (h *Handler) manifestByDigest(
	w http.ResponseWriter,
	r *http.Request,
	name string,
	dgst digest.Digest,
) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.getManifest(w, r, name, dgst)
	case http.MethodPut:
		h.putManifest(w, r, name, dgst, nil)
	case http.MethodDelete:
		h.deleteManifest(w, r, name, dgst)
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

// manifestByTag handles a reference that is a tag.
//
// A GET resolves the name and then behaves exactly as the digest form does, which
// is why it delegates rather than duplicating: the response a client gets for a
// tag must be indistinguishable from the one it gets for the digest behind it,
// down to Docker-Content-Digest.
//
// A PUT is different in kind. The tag is not a lookup key -- the manifest arrives
// in the body and does not exist yet -- so the name is something to assign after
// storing, not something to resolve first.
func (h *Handler) manifestByTag(w http.ResponseWriter, r *http.Request, name, tag string) {
	if r.Method == http.MethodPut {
		h.putManifest(w, r, name, "", []string{tag})

		return
	}

	tagged, err := h.reg.ResolveTag(r.Context(), name, tag)
	if err != nil {
		writeManifestError(w, err)

		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.getManifest(w, r, name, tagged.Digest)

	// Only the name goes, which is why this cannot delegate to the digest form: a
	// client removing a tag is not asking to remove the manifest, and other tags
	// may still point at it.
	case http.MethodDelete:
		if err := h.reg.DeleteTag(r.Context(), name, tag); err != nil {
			writeManifestError(w, err)

			return
		}

		w.WriteHeader(http.StatusAccepted)

	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

// getManifest answers end-3.
func (h *Handler) getManifest(w http.ResponseWriter, r *http.Request, name string, dgst digest.Digest) {
	// A HEAD needs only the row, so it never touches the content store. That is
	// the point of recording the media type and size: the question "does this
	// exist and what is it" is answerable from the index alone.
	if r.Method == http.MethodHead {
		manifest, err := h.reg.StatManifest(r.Context(), name, dgst)
		if err != nil {
			writeManifestError(w, err)

			return
		}

		writeManifestHeaders(w, manifest.MediaType, manifest.Digest, manifest.Size)
		w.WriteHeader(http.StatusOK)

		return
	}

	raw, manifest, err := h.reg.Manifest(r.Context(), name, dgst)
	if err != nil {
		writeManifestError(w, err)

		return
	}

	// The recorded media type, not a guess: a client dispatches on this to decide
	// whether it is holding an image, an index or an artifact.
	writeManifestHeaders(w, manifest.MediaType, manifest.Digest, int64(len(raw)))
	w.WriteHeader(http.StatusOK)

	_, _ = w.Write(raw)
}

// putManifest answers end-7a and end-7b.
//
// expected is the digest from the URL, or empty when the reference was a tag, in
// which case whatever the body hashes to is accepted. That is not a weaker check:
// the digest is still derived from the bytes, there is simply no client claim to
// compare it against.
//
// tags are the names to point at the result: the reference if it was a tag, plus
// any ?tag= parameters. Both arrive here as one list because the registry does not
// care which part of the request a name came from.
func (h *Handler) putManifest(
	w http.ResponseWriter,
	r *http.Request,
	name string,
	expected digest.Digest,
	tags []string,
) {
	tags = append(tags, r.URL.Query()["tag"]...)

	if len(tags) > maxTagsPerRequest {
		writeError(w, http.StatusRequestURITooLong, "UNSUPPORTED",
			fmt.Sprintf("at most %d tags per request", maxTagsPerRequest))

		return
	}

	manifest, err := h.reg.PutManifest(
		r.Context(), name, expected, r.Header.Get("Content-Type"), r.Body)
	if err != nil {
		writeManifestError(w, err)

		return
	}

	// Tags after the manifest, necessarily: a tag may not point at content that is
	// not there, so the order is the same "referent first" rule that governs blobs
	// before manifests.
	//
	// Not atomic with the manifest write, and the failure is benign in the one
	// direction it can go: a client that gets an error here has a stored manifest
	// and no tag, so retrying the identical request succeeds. The reverse -- a tag
	// with no manifest -- is the state that cannot happen.
	for _, tag := range tags {
		if _, err := h.reg.TagManifest(r.Context(), name, tag, manifest.Digest); err != nil {
			writeManifestError(w, err)

			return
		}
	}

	w.Header().Set("Docker-Content-Digest", manifest.Digest.String())
	w.Header().Set("Location", manifestLocation(name, manifest.Digest))

	// The spec requires this for tags created via ?tag=, and it costs nothing to be
	// consistent for a tag that came from the URL. One header line: RFC 9110 list
	// semantics make that equivalent to several, and the spec's example only splits
	// them to stay under header size limits.
	if len(tags) > 0 {
		w.Header().Set("OCI-Tag", strings.Join(tags, ", "))
	}

	// OCI 1.1: a registry that stored a subject must say so, because a client
	// that pushed a signature needs to know whether asking /referrers will find
	// it or whether it has to fall back to the tag scheme.
	if manifest.Subject != "" {
		w.Header().Set("OCI-Subject", manifest.Subject.String())
	}

	w.WriteHeader(http.StatusCreated)
}

// deleteManifest answers end-9.
func (h *Handler) deleteManifest(w http.ResponseWriter, r *http.Request, name string, dgst digest.Digest) {
	if err := h.reg.DeleteManifest(r.Context(), name, dgst); err != nil {
		writeManifestError(w, err)

		return
	}

	// 202 for the same reason as a blob: the row is gone, the bytes are not, and
	// reclaiming them is a sweep that has not run.
	w.WriteHeader(http.StatusAccepted)
}

// writeManifestHeaders sets what both a GET and a HEAD must carry.
//
// Content-Length is set explicitly rather than left to net/http, because a HEAD
// sends no body and would otherwise report no length -- and the length is most of
// what a HEAD is asked for.
func writeManifestHeaders(w http.ResponseWriter, mediaType string, dgst digest.Digest, size int64) {
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Docker-Content-Digest", dgst.String())
}

func manifestLocation(name string, dgst digest.Digest) string {
	return "/v2/" + name + "/manifests/" + dgst.String()
}

// writeManifestError maps the registry's manifest failures onto the spec's codes.
func writeManifestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "unknown manifest")
	case errors.Is(err, registry.ErrManifestBlobUnknown):
		// 404 with the client's own message: the request is well formed, and what
		// is missing is content the client is expected to push first. The detail
		// names which descriptor, since a manifest may reference many.
		writeError(w, http.StatusNotFound, "MANIFEST_BLOB_UNKNOWN", err.Error())
	case errors.Is(err, registry.ErrManifestInvalid):
		writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
	case errors.Is(err, registry.ErrTagInvalid):
		writeError(w, http.StatusBadRequest, "UNSUPPORTED", err.Error())
	case errors.Is(err, registry.ErrDigestMismatch):
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
	case errors.Is(err, registry.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", err.Error())
	default:
		writeServerError(w, err)
	}
}
