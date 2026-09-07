package ocihttp

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/tkircsi/cairn/internal/registry"
)

// referrers answers end-12a and end-12b.
//
// The response is an image index that is *assembled*, not stored: no client ever
// pushed it and its digest is not stable, because the next signature pushed
// changes it. That is the one place this API departs from content addressing, and
// it is the reason the endpoint exists at all -- the answer to "what refers to
// this" is not a property of the subject's bytes.
//
// end-12b is the same endpoint with an artifactType filter, so there is nothing to
// route between them.
func (h *Handler) referrers(w http.ResponseWriter, r *http.Request, name, reference string) {
	// GET only. HEAD is not offered for the same reason end-8a does not: the body
	// has to be built to know its length, so a HEAD would cost a GET and save only
	// the write.
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")

		return
	}

	// A tag is not accepted here, unlike the manifest endpoints. The spec's path is
	// <digest>, and the reason is not pedantry: the subject a referrer recorded is a
	// digest, so resolving a tag first would answer about whatever the tag points at
	// *now* while the referrers were filed against what it pointed at then. A
	// signature would appear to vanish the moment a tag moved.
	subject, err := digest.Parse(reference)
	if err != nil {
		if errors.Is(err, digest.ErrDigestUnsupported) {
			writeError(w, http.StatusBadRequest, "UNSUPPORTED", "unsupported digest algorithm")

			return
		}

		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "reference must be a digest")

		return
	}

	artifactType, err := artifactTypeFilter(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "UNSUPPORTED", err.Error())

		return
	}

	referrers, err := h.reg.Referrers(r.Context(), name, subject, artifactType)
	if err != nil {
		writeReferrersError(w, err)

		return
	}

	// Declared only when it actually narrowed the query, which is what lets a client
	// tell "your filter was applied" from "this registry ignored it and you must
	// filter locally". A registry is allowed to ignore the parameter, so its absence
	// is meaningful and must not be sent unconditionally.
	if artifactType != "" {
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}

	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		// Non-nil, so an empty result marshals as [] and not null. "Nothing refers to
		// this" is the most common answer this endpoint gives, and a client that has
		// to defend against null for it will not.
		Manifests: make([]ocispec.Descriptor, 0, len(referrers)),
	}

	for _, m := range referrers {
		index.Manifests = append(index.Manifests, ocispec.Descriptor{
			MediaType: m.MediaType,
			Digest:    m.Digest,
			Size:      m.Size,
			// Already resolved at push time, including the spec's fallback to the config
			// media type for an image manifest that declared none. Empty here means an
			// index that declared none, and omitempty drops it -- which is what the spec
			// asks for in that one case.
			ArtifactType: m.ArtifactType,
			// Reproduced because they are the only human-readable thing in the response.
			// Without them a client looking for one signature among twenty has to fetch
			// all twenty to find out which is which, which is the read amplification this
			// endpoint exists to prevent.
			Annotations: m.Annotations,
		})
	}

	// The spec fixes the Content-Type to the image index type, even though the body
	// is synthesised and no such index was ever pushed.
	writeJSONAs(w, http.StatusOK, ocispec.MediaTypeImageIndex, index)
}

// artifactTypeFilter reads the artifactType parameter out of the raw query,
// percent-decoding it but leaving "+" alone. An empty result means no filter.
//
// This is deliberately not r.URL.Query().Get. Go's query parser follows the HTML
// form convention where "+" means a space, and almost every artifact type ends in
// "+json" -- so a client sending the perfectly legal
// "?artifactType=application/vnd.example.sbom.v1+json" would be filtering on a
// type with a space in it and get an empty list back. RFC 3986 does not require
// "+" to be escaped in a query, so that client is not wrong.
//
// The reason it is safe to reinterpret is that only one reading can ever be
// correct: a media type cannot contain a space, so there is no input for which
// form decoding would have been right. A properly escaped "%2B" still arrives as
// "+", so clients that do escape are unaffected.
//
// The failure mode this avoids is the dangerous kind. An empty list is a
// well-formed answer meaning "nothing has been said about this", so a verification
// tool would read a mangled filter as "unsigned" rather than as an error.
//
// Present but empty is treated as absent. Filtering on the empty string could only
// ever match nothing, since no manifest has an empty effective artifact type.
//
// The first occurrence wins if the parameter repeats; the spec defines one filter,
// and combining them would have to invent whether they mean AND or OR.
func artifactTypeFilter(rawQuery string) (string, error) {
	for rawQuery != "" {
		var pair string

		pair, rawQuery, _ = strings.Cut(rawQuery, "&")

		key, value, _ := strings.Cut(pair, "=")
		if key != "artifactType" {
			continue
		}

		// PathUnescape rather than QueryUnescape: the two differ only in their
		// treatment of "+", which is the entire point.
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return "", fmt.Errorf("artifactType is not a valid encoded value: %w", err)
		}

		return decoded, nil
	}

	return "", nil
}

func writeReferrersError(w http.ResponseWriter, err error) {
	switch {
	// A repository that holds nothing at all, and the one 404 this endpoint can
	// produce.
	//
	// This is a reading, not a transcription. The spec says a registry supporting
	// this API "MUST NOT return a 404 Not Found to a referrers API request", yet
	// end-12a lists 404 among its failure codes -- so the prohibition cannot be
	// absolute. Taking it to mean "not for a subject with no referrers" fits both
	// sentences, and the alternative is worse where it matters: a mistyped
	// repository would answer 200 with an empty list, and a verification tool
	// cannot distinguish that from "this image is unsigned".
	//
	// The cost of being wrong is bounded. A client that reads 404 as "no referrers
	// API here" falls back to the referrers tag schema, which in a repository that
	// does not exist is also a 404, so it arrives at the same answer either way.
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository unknown")
	default:
		writeServerError(w, err)
	}
}
