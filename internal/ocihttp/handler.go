// Package ocihttp serves the blob and manifest endpoints of the OCI distribution
// API.
//
// Endpoints implemented, by the spec's own numbering:
//
//	end-1   GET    /v2/
//	end-2   GET    /v2/<name>/blobs/<digest>            (also HEAD, with Range)
//	end-3   GET    /v2/<name>/manifests/<digest>        (also HEAD)
//	end-4a  POST   /v2/<name>/blobs/uploads/
//	end-4b  POST   /v2/<name>/blobs/uploads/?digest=    single-request push
//	end-5   PATCH  /v2/<name>/blobs/uploads/<ref>       one chunk
//	end-6   PUT    /v2/<name>/blobs/uploads/<ref>?digest=
//	end-7a  PUT    /v2/<name>/manifests/<reference>
//	end-7b  PUT    /v2/<name>/manifests/<digest>?tag=&tag=
//	end-8a  GET    /v2/<name>/tags/list
//	end-8b  GET    /v2/<name>/tags/list?n=&last=
//	end-9   DELETE /v2/<name>/manifests/<reference>
//	end-10  DELETE /v2/<name>/blobs/<digest>
//	end-11  POST   /v2/<name>/blobs/uploads/?mount=&from=
//	end-12a GET    /v2/<name>/referrers/<digest>
//	end-12b GET    /v2/<name>/referrers/<digest>?artifactType=
//	end-13  GET    /v2/<name>/blobs/uploads/<ref>       session status
//
// A manifest reference is a digest or a tag. Blob and referrers references are
// always digests. For a blob that is not an omission -- it is opaque and has no
// name but its content -- and for referrers it is deliberate, for the reason given
// in referrers.go.
//
// Manifest endpoints live in manifest.go, tag listing in tags.go, referrers in
// referrers.go.
//
// The handler's whole job is translation: it parses a request into the arguments
// the registry takes, and turns the registry's outcomes into the status and error
// codes the spec assigns them. No storage decision is made here.
package ocihttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/tkircsi/cairn/internal/registry"
	"github.com/tkircsi/cairn/internal/uploadstore"
)

// nameRE is the repository name grammar from the spec.
//
// Anchored, because an unanchored match would accept any string containing a
// legal name. Names never become filesystem paths here -- they are only ever
// bound as SQL parameters -- but validating at the edge keeps a malformed name
// from reaching storage at all.
var nameRE = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)

// maxNameLength bounds the name before it is matched or stored.
const maxNameLength = 255

// Handler serves the API over a Registry.
type Handler struct {
	reg *registry.Registry
}

// NewHandler builds a Handler.
func NewHandler(reg *registry.Registry) *Handler {
	return &Handler{reg: reg}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every response carries this: it is how a client confirms it is talking to a
	// v2 registry rather than something that merely answers /v2/ with a 200.
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")

	path := r.URL.Path

	if path == "/v2" || path == "/v2/" {
		h.version(w, r)

		return
	}

	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		writeError(w, http.StatusNotFound, "UNSUPPORTED", "not a registry endpoint")

		return
	}

	name, section, tail, ok := splitPath(rest)
	if !ok {
		writeError(w, http.StatusNotFound, "UNSUPPORTED", "not a registry endpoint")

		return
	}

	if len(name) > maxNameLength || !nameRE.MatchString(name) {
		writeError(w, http.StatusBadRequest, "NAME_INVALID", "invalid repository name")

		return
	}

	switch section {
	case sectionBlobs:
		h.blobs(w, r, name, tail)
	case sectionManifests:
		h.manifest(w, r, name, tail)
	case sectionTags:
		h.tags(w, r, name, tail)
	case sectionReferrers:
		h.referrers(w, r, name, tail)
	}
}

// Route sections, which are the only path components after a repository name.
var sections = []string{sectionBlobs, sectionManifests, sectionTags, sectionReferrers}

const (
	sectionBlobs     = "/blobs"
	sectionManifests = "/manifests"
	sectionTags      = "/tags"
	sectionReferrers = "/referrers"
)

// splitPath separates a repository name from the endpoint acting on it.
//
// A repository name contains slashes, so the route cannot simply be split on
// them, and a name may legally end in a component called "blobs", "manifests" or
// "tags". Taking the *last* occurrence of any section marker resolves that
// ambiguity in favour of the endpoint, which is what other registries do, and
// taking the latest of the markers extends the rule to a name containing several.
func splitPath(rest string) (name, section, tail string, ok bool) {
	marker, at := "", -1

	for _, candidate := range sections {
		if found := strings.LastIndex(rest, candidate); found > at {
			marker, at = candidate, found
		}
	}

	if at < 0 {
		return "", "", "", false
	}

	return rest[:at], marker, strings.TrimPrefix(rest[at+len(marker):], "/"), true
}

// blobs routes the blob endpoints beneath a repository.
func (h *Handler) blobs(w http.ResponseWriter, r *http.Request, name, tail string) {
	switch {
	// The spec writes this endpoint with a trailing slash and clients send it
	// that way, but the slash is not a separator here -- there is no session
	// identifier yet.
	case tail == "uploads", tail == "uploads/":
		h.startUpload(w, r, name)
	case strings.HasPrefix(tail, "uploads/"):
		h.session(w, r, name, strings.TrimPrefix(tail, "uploads/"))
	case tail != "":
		h.blob(w, r, name, tail)
	default:
		writeError(w, http.StatusNotFound, "UNSUPPORTED", "not a registry endpoint")
	}
}

// version answers end-1, the check a client makes before anything else.
func (h *Handler) version(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte("{}"))
	}
}

// startUpload answers end-4a, end-4b and end-11, which share a URL and are told
// apart by their query parameters.
func (h *Handler) startUpload(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")

		return
	}

	query := r.URL.Query()

	if raw := query.Get("mount"); raw != "" {
		h.mount(w, r, name, raw, query.Get("from"))

		return
	}

	// A digest on the POST means the body is the entire blob and the session is
	// an implementation detail the client never sees.
	if raw := query.Get("digest"); raw != "" {
		h.monolithic(w, r, name, raw)

		return
	}

	upload, err := h.reg.StartUpload(r.Context(), name)
	if err != nil {
		writeServerError(w, err)

		return
	}

	writeSession(w, name, upload.ID, upload.Received, http.StatusAccepted)
}

// mount answers end-11. A blob already on disk is granted to a second repository
// by writing one row; nothing is transferred.
func (h *Handler) mount(w http.ResponseWriter, r *http.Request, name, rawDigest, from string) {
	dgst, err := digest.Parse(rawDigest)
	if err == nil {
		blob, mountErr := h.reg.MountBlob(r.Context(), name, from, dgst)
		if mountErr == nil {
			w.Header().Set("Docker-Content-Digest", blob.Digest.String())
			w.Header().Set("Location", blobLocation(name, blob.Digest))
			w.WriteHeader(http.StatusCreated)

			return
		}

		if !errors.Is(mountErr, registry.ErrNotFound) {
			writeServerError(w, mountErr)

			return
		}
	}

	// The spec's fallback: an unmountable blob is not an error, it is a normal
	// upload. Answering 202 here lets the client push the bytes it already has
	// instead of having to interpret a failure.
	upload, err := h.reg.StartUpload(r.Context(), name)
	if err != nil {
		writeServerError(w, err)

		return
	}

	writeSession(w, name, upload.ID, upload.Received, http.StatusAccepted)
}

// monolithic answers end-4b: open a session, close it with the body, report the
// blob. The two round trips of end-4a plus end-6 collapse into one.
func (h *Handler) monolithic(w http.ResponseWriter, r *http.Request, name, rawDigest string) {
	dgst, err := digest.Parse(rawDigest)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest")

		return
	}

	upload, err := h.reg.StartUpload(r.Context(), name)
	if err != nil {
		writeServerError(w, err)

		return
	}

	blob, err := h.reg.CompleteUpload(r.Context(), upload.ID, r.Body, dgst)
	if err != nil {
		// The session is this handler's own bookkeeping, so a failed push must not
		// leave one behind for a client that has no idea it existed.
		_ = h.reg.CancelUpload(r.Context(), upload.ID)

		writeUploadError(w, err)

		return
	}

	w.Header().Set("Docker-Content-Digest", blob.Digest.String())
	w.Header().Set("Location", blobLocation(name, blob.Digest))
	w.WriteHeader(http.StatusCreated)
}

// session routes the methods that act on an open upload.
func (h *Handler) session(w http.ResponseWriter, r *http.Request, name, id string) {
	// Checked before the store is touched, so an identifier that could not have
	// been issued never reaches it.
	if !uploadstore.ValidID(id) {
		writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload")

		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.uploadStatus(w, r, name, id)
	case http.MethodPatch:
		h.appendChunk(w, r, name, id)
	case http.MethodPut:
		h.completeUpload(w, r, name, id)
	case http.MethodDelete:
		h.cancelUpload(w, r, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

// uploadStatus answers end-13, which is how a client resumes after a failure: it
// asks how much arrived rather than starting over.
func (h *Handler) uploadStatus(w http.ResponseWriter, r *http.Request, name, id string) {
	upload, err := h.reg.UploadStatus(r.Context(), id)
	if err != nil {
		writeUploadError(w, err)

		return
	}

	writeSession(w, name, upload.ID, upload.Received, http.StatusNoContent)
}

// appendChunk answers end-5.
func (h *Handler) appendChunk(w http.ResponseWriter, r *http.Request, name, id string) {
	start, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", err.Error())

		return
	}

	upload, err := h.reg.AppendUpload(r.Context(), id, start, r.Body)
	if err != nil {
		writeUploadError(w, err)

		return
	}

	writeSession(w, name, upload.ID, upload.Received, http.StatusAccepted)
}

// completeUpload answers end-6. The digest is mandatory here: it is the only
// thing that lets the registry decide whether what it assembled is what the
// client meant to send.
func (h *Handler) completeUpload(w http.ResponseWriter, r *http.Request, name, id string) {
	raw := r.URL.Query().Get("digest")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest query parameter is required")

		return
	}

	dgst, err := digest.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest")

		return
	}

	blob, err := h.reg.CompleteUpload(r.Context(), id, r.Body, dgst)
	if err != nil {
		writeUploadError(w, err)

		return
	}

	w.Header().Set("Docker-Content-Digest", blob.Digest.String())
	w.Header().Set("Location", blobLocation(name, blob.Digest))
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) cancelUpload(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.reg.CancelUpload(r.Context(), id); err != nil {
		writeUploadError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// blob answers end-2 and end-10.
func (h *Handler) blob(w http.ResponseWriter, r *http.Request, name, reference string) {
	dgst, err := digest.Parse(reference)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest")

		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.getBlob(w, r, name, dgst)
	case http.MethodDelete:
		h.deleteBlob(w, r, name, dgst)
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

func (h *Handler) getBlob(w http.ResponseWriter, r *http.Request, name string, dgst digest.Digest) {
	content, blob, err := h.reg.OpenBlob(r.Context(), name, dgst)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "unknown blob")

			return
		}

		writeServerError(w, err)

		return
	}

	defer content.Close()

	// Set before ServeContent, which sniffs the body only when Content-Type is
	// absent. A blob is opaque bytes, so sniffing it would be a guess a client
	// might then act on.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", dgst.String())

	// ServeContent brings Range, If-Range, 206 and 416 with it, and skips the
	// body on HEAD while still reporting Content-Length. Range support is a SHOULD
	// in end-2 and is what makes a resumable pull possible.
	http.ServeContent(w, r, "", blob.CreatedAt, content)
}

func (h *Handler) deleteBlob(w http.ResponseWriter, r *http.Request, name string, dgst digest.Digest) {
	if err := h.reg.DeleteBlob(r.Context(), name, dgst); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "unknown blob")

			return
		}

		writeServerError(w, err)

		return
	}

	// 202, not 204: only the membership row is gone. Reclaiming the bytes is a
	// separate sweep, so the deletion really is merely accepted.
	w.WriteHeader(http.StatusAccepted)
}

// parseContentRange reads the start offset a chunk claims.
//
// An absent header is legal -- a streamed upload appends wherever the session
// happens to be -- and is reported as -1 so the registry can tell "no claim"
// from "claims to start at zero".
func parseContentRange(header string) (int64, error) {
	if header == "" {
		return -1, nil
	}

	// The spec writes the value as "<start>-<end>", but clients that reuse an
	// HTTP range formatter send the "bytes " prefix, and rejecting them would be
	// pedantry.
	value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "bytes"))
	value = strings.TrimSpace(value)

	first, last, ok := strings.Cut(value, "-")
	if !ok {
		return 0, fmt.Errorf("malformed Content-Range %q", header)
	}

	start, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil || start < 0 {
		return 0, fmt.Errorf("malformed Content-Range %q", header)
	}

	end, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64)
	if err != nil || end < start {
		return 0, fmt.Errorf("malformed Content-Range %q", header)
	}

	return start, nil
}

// writeSession emits the headers that describe an open upload. Every response
// that leaves a session open carries the same three, so they are built in one
// place: a client resumes from Range and addresses the session through Location.
func writeSession(w http.ResponseWriter, name, id string, received int64, status int) {
	w.Header().Set("Location", sessionLocation(name, id))
	w.Header().Set("Range", rangeHeader(received))
	w.Header().Set("Docker-Upload-UUID", id)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

// rangeHeader formats the inclusive position of the last byte held, which is what
// the spec means by <offset>. An empty session has no last byte; "0-0" is what
// clients expect there.
func rangeHeader(received int64) string {
	if received <= 0 {
		return "0-0"
	}

	return fmt.Sprintf("0-%d", received-1)
}

func sessionLocation(name, id string) string {
	return "/v2/" + name + "/blobs/uploads/" + id
}

func blobLocation(name string, dgst digest.Digest) string {
	return "/v2/" + name + "/blobs/" + dgst.String()
}

// writeUploadError maps the registry's session failures onto the spec's codes.
func writeUploadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrUploadUnknown):
		writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload")
	case errors.Is(err, registry.ErrDigestMismatch):
		// The client's claim about its own bytes was wrong, so this is a bad
		// request rather than a failure of the store.
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
	case errors.Is(err, registry.ErrNotContiguous):
		// 416 is the spec's answer here, and it is the accurate one: the range the
		// client asked to write is not one the session can satisfy.
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", err.Error())
	case errors.Is(err, registry.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", err.Error())
	default:
		writeServerError(w, err)
	}
}

// faultRecorder is implemented by the access log's response recorder, and is how the
// cause of a 500 reaches the log without reaching the client.
//
// An interface rather than a concrete type so a Handler still works when it is not
// wrapped -- as it is not in most of the tests -- and so another wrapper can opt in.
type faultRecorder interface {
	recordFault(error)
}

// writeServerError reports a fault to the client without describing it, and to the log
// with the description.
//
// The detail goes nowhere a client can see: an internal error message can carry
// filesystem paths or SQL text, and a caller can do nothing with either. But discarding
// it entirely, which is what this did at first, means a 500 leaves no trace of its cause
// anywhere -- the access log records that the request failed and the reason is gone. That
// cost real time: an ordinary `oras push` was failing on a concurrent write, and the only
// evidence was a status code.
func writeServerError(w http.ResponseWriter, err error) {
	if recorder, ok := w.(faultRecorder); ok {
		recorder.recordFault(err)
	}

	writeError(w, http.StatusInternalServerError, "UNSUPPORTED", "internal error")
}

// ociError is the spec's error envelope. Clients parse this, so the shape is not
// ours to choose.
type ociError struct {
	Errors []ociErrorDetail `json:"errors"`
}

type ociErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(ociError{
		Errors: []ociErrorDetail{{Code: code, Message: message}},
	})
}
