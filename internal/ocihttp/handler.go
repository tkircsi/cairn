// Package ocihttp serves the read-only OCI surface.
//
// Scope is deliberate: end-1 so clients can detect the API, and end-12a/12b so
// they can discover referrers. There is no push path, because the store is
// written through its own API where content can be validated. That asymmetry is
// the design, not an unfinished edge -- it also happens to skip the fiddliest
// part of the spec, the resumable blob-upload session.
package ocihttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/tkircsi/cairn/internal/registry"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// Handler serves the OCI read endpoints.
type Handler struct {
	reg *registry.Registry
	mux *http.ServeMux
}

// NewHandler wires the routes.
func NewHandler(reg *registry.Registry) *Handler {
	h := &Handler{reg: reg, mux: http.NewServeMux()}

	// end-1: the version check. Without it no client will proceed to anything
	// else, which is why a read-only registry still needs it.
	h.mux.HandleFunc("GET /v2/", h.route)

	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// route dispatches by hand rather than by pattern because <name> may contain
// slashes ("project/repo" on Harbor, "library/nginx" on Docker Hub) while the
// path segments after it are fixed. A ServeMux wildcard cannot express a
// multi-segment parameter that is followed by more literal segments.
func (h *Handler) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v2/")

	if path == "" {
		h.version(w)

		return
	}

	const marker = "/referrers/"

	if idx := strings.LastIndex(path, marker); idx > 0 {
		name := path[:idx]
		reference := path[idx+len(marker):]

		if name != "" && reference != "" {
			h.referrers(w, r, name, reference)

			return
		}
	}

	writeError(w, http.StatusNotFound, "NOT_FOUND", "unsupported endpoint")
}

func (h *Handler) version(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("{}"))
}

// referrers implements end-12a and end-12b.
func (h *Handler) referrers(w http.ResponseWriter, r *http.Request, name, reference string) {
	subject, err := digest.Parse(reference)
	if err != nil {
		// The one case the spec requires to fail: invalid digest syntax.
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest syntax")

		return
	}

	query := r.URL.Query()
	artifactType := query.Get("artifactType")

	pageSize := defaultPageSize

	if raw := query.Get("n"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "UNSUPPORTED", "invalid n parameter")

			return
		}

		pageSize = min(parsed, maxPageSize)
	}

	var after digest.Digest

	if raw := query.Get("last"); raw != "" {
		parsed, err := digest.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid last parameter")

			return
		}

		after = parsed
	}

	page, err := h.reg.Referrers(r.Context(), name, subject, artifactType, pageSize, after)
	if err != nil {
		// An absent subject is not an error here; only a backend failure is.
		if errors.Is(err, registry.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository not found")

			return
		}

		writeError(w, http.StatusInternalServerError, "UNSUPPORTED", "failed to list referrers")

		return
	}

	body, err := json.Marshal(page.Index)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "UNSUPPORTED", "failed to encode index")

		return
	}

	// The spec fixes this Content-Type for the endpoint; clients use it to
	// confirm they received an index rather than something else.
	w.Header().Set("Content-Type", ocispec.MediaTypeImageIndex)

	// Required when a filter was requested and honoured, so a client knows the
	// result is filtered rather than complete.
	if artifactType != "" {
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}

	if page.Next != "" {
		w.Header().Set("Link", nextLink(name, subject, artifactType, pageSize, page.Next))
	}

	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// nextLink builds the RFC 5988 Link header the spec requires on a truncated page.
func nextLink(name string, subject digest.Digest, artifactType string, pageSize int, next digest.Digest) string {
	query := url.Values{}
	query.Set("n", strconv.Itoa(pageSize))
	query.Set("last", next.String())

	if artifactType != "" {
		query.Set("artifactType", artifactType)
	}

	target := url.URL{
		Path:     "/v2/" + name + "/referrers/" + subject.String(),
		RawQuery: query.Encode(),
	}

	return "<" + target.String() + `>; rel="next"`
}

// ociError is the error envelope every OCI endpoint must use.
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

	json.NewEncoder(w).Encode(ociError{
		Errors: []ociErrorDetail{{Code: code, Message: message}},
	})
}
