package ocihttp

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/opencontainers/go-digest"

	"github.com/tkircsi/cairn/internal/registry"
)

// tagRE is the tag grammar from the spec.
//
// Tags are not served here, but they are recognised so that a client using one
// gets told that rather than being told its manifest does not exist.
var tagRE = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

// manifest routes end-3, end-7 and end-9, which share a URL.
func (h *Handler) manifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	dgst, err := digest.Parse(reference)
	if err != nil {
		switch {
		// A well-formed tag is a request this registry understands but does not
		// serve, and UNSUPPORTED is the spec's code for exactly that. Answering 404
		// instead would be a lie a client could act on -- it would conclude the
		// manifest is absent and try to push it.
		case tagRE.MatchString(reference):
			writeError(w, http.StatusBadRequest, "UNSUPPORTED",
				"tag references are not implemented; use a digest")

		// A named algorithm this build does not implement is also a "no" rather
		// than a "that is gibberish", and go-digest tells the two apart, so there
		// is no reason to collapse them.
		case errors.Is(err, digest.ErrDigestUnsupported):
			writeError(w, http.StatusBadRequest, "UNSUPPORTED", "unsupported digest algorithm")

		default:
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID",
				"reference is neither a digest nor a valid tag")
		}

		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.getManifest(w, r, name, dgst)
	case http.MethodPut:
		h.putManifest(w, r, name, dgst)
	case http.MethodDelete:
		h.deleteManifest(w, r, name, dgst)
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

// putManifest answers end-7.
func (h *Handler) putManifest(w http.ResponseWriter, r *http.Request, name string, dgst digest.Digest) {
	manifest, err := h.reg.PutManifest(r.Context(), name, dgst, r.Header.Get("Content-Type"), r.Body)
	if err != nil {
		writeManifestError(w, err)

		return
	}

	w.Header().Set("Docker-Content-Digest", manifest.Digest.String())
	w.Header().Set("Location", manifestLocation(name, manifest.Digest))

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
	case errors.Is(err, registry.ErrDigestMismatch):
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
	case errors.Is(err, registry.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", err.Error())
	default:
		writeServerError(w, err)
	}
}
