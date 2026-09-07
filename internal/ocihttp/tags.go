package ocihttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/tkircsi/cairn/internal/registry"
)

// tagList is the response body for end-8a.
//
// tags is always non-nil where this is built, because the spec shows a list and a
// nil slice would marshal as null -- a difference clients have historically
// crashed on.
type tagList struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// tags routes end-8a and end-8b.
func (h *Handler) tags(w http.ResponseWriter, r *http.Request, name, tail string) {
	if tail != "list" {
		writeError(w, http.StatusNotFound, "UNSUPPORTED", "not a registry endpoint")

		return
	}

	// GET only. HEAD is not offered because answering one honestly means building
	// the whole body to know its length, which is the entire cost of the GET.
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")

		return
	}

	query := r.URL.Query()

	limit, err := pageLimit(query.Get("n"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "UNSUPPORTED", err.Error())

		return
	}

	names, more, err := h.reg.Tags(r.Context(), name, query.Get("last"), limit)
	if err != nil {
		writeTagError(w, err)

		return
	}

	// Only when a page was capped can there be another, so this is the one place a
	// Link is possible. A request without n returns everything and therefore has no
	// next page to point at.
	if more {
		w.Header().Set("Link", nextPageLink(name, limit, names[len(names)-1]))
	}

	writeJSON(w, http.StatusOK, tagList{Name: name, Tags: names})
}

// pageLimit turns the n parameter into a limit for the registry.
//
// Absent n means unlimited, which is end-8a. A present n is honoured exactly,
// including zero: the spec requires n=0 to produce an empty list and no Link
// header. Note that n=0 is still routed through the registry rather than answered
// here, because a nonexistent repository must 404 whatever n says.
func pageLimit(raw string) (int, error) {
	if raw == "" {
		return -1, nil
	}

	limit, err := strconv.Atoi(raw)

	// Refused rather than ignored. A client sends n to bound the response it is
	// willing to read, so quietly dropping it could hand back a body far larger
	// than it asked for -- the one outcome it was trying to prevent.
	if err != nil || limit < 0 {
		return 0, errors.New("n must be a non-negative integer")
	}

	return limit, nil
}

// nextPageLink builds the rel="next" Link for end-8b.
//
// The cursor is the last tag returned, not an offset, so a tag pushed or deleted
// between pages cannot shift the window and cause a client to skip or repeat one.
// Both values are query-escaped because a tag may contain characters -- a dot, a
// hyphen -- that are legal in a tag and meaningful in a URL.
func nextPageLink(name string, limit int, last string) string {
	next := url.Values{}
	next.Set("n", strconv.Itoa(limit))
	next.Set("last", last)

	return "</v2/" + name + "/tags/list?" + next.Encode() + `>; rel="next"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		writeServerError(w, err)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(status)

	_, _ = w.Write(raw)
}

func writeTagError(w http.ResponseWriter, err error) {
	switch {
	// A repository holding nothing at all, which the spec separates from a
	// repository that merely has no tags. NAME_UNKNOWN rather than
	// MANIFEST_UNKNOWN: what is missing is the namespace, not a document in it.
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository unknown")
	case errors.Is(err, registry.ErrTagInvalid):
		writeError(w, http.StatusBadRequest, "UNSUPPORTED", err.Error())
	default:
		writeServerError(w, err)
	}
}
