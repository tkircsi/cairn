package ocihttp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// tagsOf reads a tag list response, failing on anything but a 200.
func tagsOf(t *testing.T, resp *http.Response) (name string, tags []string) {
	t.Helper()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, errorCode(t, resp))
	}

	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode tag list: %v", err)
	}

	return body.Name, body.Tags
}

// tagged pushes a manifest under a tag and returns the digest it landed at.
func tagged(t *testing.T, server *httptest.Server, repository, tag string) string {
	t.Helper()

	raw, dgst := imageManifest(t, server, repository)

	resp := do(t, server, http.MethodPut,
		"/v2/"+repository+"/manifests/"+tag, strings.NewReader(string(raw)),
		map[string]string{"Content-Type": imageManifestType})

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT %s: status = %d, want 201: %s", tag, resp.StatusCode, errorCode(t, resp))
	}

	if got := resp.Header.Get("Docker-Content-Digest"); got != dgst.String() {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, dgst)
	}

	return dgst.String()
}

// TestTagRoundTrip is the point of tags: content becomes reachable without the
// caller knowing its digest, and the answer is identical to the digest form.
func TestTagRoundTrip(t *testing.T) {
	server := newServer(t)

	want := tagged(t, server, repo, "v1.0.0")

	byTag := do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/v1.0.0", nil, nil)
	if byTag.StatusCode != http.StatusOK {
		t.Fatalf("GET by tag: status = %d, want 200", byTag.StatusCode)
	}

	if got := byTag.Header.Get("Docker-Content-Digest"); got != want {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, want)
	}

	if got := byTag.Header.Get("Content-Type"); got != imageManifestType {
		t.Errorf("Content-Type = %q, want %q", got, imageManifestType)
	}

	// A HEAD by tag must work too, and for the same reason as by digest: it is
	// answered from the index without reading content.
	head := do(t, server, http.MethodHead, "/v2/"+repo+"/manifests/v1.0.0", nil, nil)
	if head.StatusCode != http.StatusOK {
		t.Errorf("HEAD by tag: status = %d, want 200", head.StatusCode)
	}

	if got := head.Header.Get("Docker-Content-Digest"); got != want {
		t.Errorf("HEAD digest = %q, want %q", got, want)
	}
}

// TestTagMoves covers the one write in this store that destroys information.
func TestTagMoves(t *testing.T) {
	server := newServer(t)

	first := tagged(t, server, repo, "latest")

	// A second, different manifest in the same repository. A distinct config makes
	// distinct bytes and therefore a distinct digest.
	config := descriptorFor(t, server, repo, emptyConfigType, []byte(`{"v":2}`))

	raw, second := encode(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     imageManifestType,
		"config":        config,
		"layers":        []any{},
	})

	if first == second.String() {
		t.Fatal("test is not exercising a move: both manifests have the same digest")
	}

	resp := do(t, server, http.MethodPut, "/v2/"+repo+"/manifests/latest",
		strings.NewReader(string(raw)), map[string]string{"Content-Type": imageManifestType})

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("re-tag: status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	resp = do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/latest", nil, nil)
	if got := resp.Header.Get("Docker-Content-Digest"); got != second.String() {
		t.Errorf("tag did not move: digest = %q, want %q", got, second)
	}

	// The manifest the tag used to name is untouched. Moving a name is not deleting
	// content, and anything holding the old digest must still resolve.
	resp = do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/"+first, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("previously tagged manifest is gone: %d", resp.StatusCode)
	}

	// And the tag appears once, not twice.
	_, tags := tagsOf(t, do(t, server, http.MethodGet, "/v2/"+repo+"/tags/list", nil, nil))
	if len(tags) != 1 || tags[0] != "latest" {
		t.Errorf("tags = %v, want [latest]", tags)
	}
}

// TestMultipleTagsInOneRequest covers end-7b, which is how a release acquires
// 1.2.3, 1.2 and latest without three round trips.
func TestMultipleTagsInOneRequest(t *testing.T) {
	server := newServer(t)

	raw, dgst := imageManifest(t, server, repo)

	resp := do(t, server, http.MethodPut,
		fmt.Sprintf("/v2/%s/manifests/%s?tag=1.2.3&tag=1.2&tag=latest", repo, dgst),
		strings.NewReader(string(raw)),
		map[string]string{"Content-Type": imageManifestType})

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, errorCode(t, resp))
	}

	// The spec requires the accepted tags to be reported back, because a registry
	// is allowed to support fewer than were asked for.
	reported := resp.Header.Get("OCI-Tag")
	for _, tag := range []string{"1.2.3", "1.2", "latest"} {
		if !strings.Contains(reported, tag) {
			t.Errorf("OCI-Tag %q does not report %q", reported, tag)
		}
	}

	_, tags := tagsOf(t, do(t, server, http.MethodGet, "/v2/"+repo+"/tags/list", nil, nil))

	if want := []string{"1.2", "1.2.3", "latest"}; !equal(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}

	// All three resolve to the same manifest, which is the point: they are names
	// for one immutable document, not three copies of it.
	for _, tag := range tags {
		resp := do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/"+tag, nil, nil)

		if got := resp.Header.Get("Docker-Content-Digest"); got != dgst.String() {
			t.Errorf("tag %s resolves to %q, want %q", tag, got, dgst)
		}
	}
}

func TestTooManyTags(t *testing.T) {
	server := newServer(t)

	raw, dgst := imageManifest(t, server, repo)

	query := url.Values{}
	for i := 0; i < 100; i++ {
		query.Add("tag", fmt.Sprintf("v%d", i))
	}

	resp := do(t, server, http.MethodPut,
		fmt.Sprintf("/v2/%s/manifests/%s?%s", repo, dgst, query.Encode()),
		strings.NewReader(string(raw)),
		map[string]string{"Content-Type": imageManifestType})

	if resp.StatusCode != http.StatusRequestURITooLong {
		t.Errorf("status = %d, want 414", resp.StatusCode)
	}
}

// TestTagListOrder pins the ordering the spec requires, which is also what makes
// the "last" cursor work: pagination is only coherent over a total order clients
// can predict.
func TestTagListOrder(t *testing.T) {
	server := newServer(t)

	// Deliberately including case variants and a numeric-looking set, because
	// "lexical" is not "numeric": v10 sorts before v9.
	pushed := []string{"v9", "v10", "Beta", "alpha", "v1.0.0", "_internal"}
	for _, tag := range pushed {
		tagged(t, server, repo, tag)
	}

	name, tags := tagsOf(t, do(t, server, http.MethodGet, "/v2/"+repo+"/tags/list", nil, nil))

	if name != repo {
		t.Errorf("name = %q, want %q", name, repo)
	}

	want := append([]string(nil), pushed...)
	sort.Strings(want) // the order the spec names, by reference to this function

	if !equal(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
}

// TestTagListPagination walks every page and checks the walk is exact: each tag
// seen once, in order, with the Link header driving it.
func TestTagListPagination(t *testing.T) {
	server := newServer(t)

	want := []string{"a", "b", "c", "d", "e"}
	for _, tag := range want {
		tagged(t, server, repo, tag)
	}

	var (
		seen []string
		path = "/v2/" + repo + "/tags/list?n=2"
		hops int
	)

	for path != "" {
		resp := do(t, server, http.MethodGet, path, nil, nil)

		_, page := tagsOf(t, resp)
		seen = append(seen, page...)

		path = nextFromLink(t, resp.Header.Get("Link"))

		if hops++; hops > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	if !equal(seen, want) {
		t.Errorf("walked %v, want %v", seen, want)
	}
}

// TestTagListLastIsACursorNotAFilter covers the property that makes keyset
// pagination safe: the cursor names a position, so it need not still exist.
func TestTagListLastIsACursorNotAFilter(t *testing.T) {
	server := newServer(t)

	for _, tag := range []string{"a", "b", "c"} {
		tagged(t, server, repo, tag)
	}

	// Delete the tag a client is about to paginate from, imitating a concurrent
	// change between two pages.
	resp := do(t, server, http.MethodDelete, "/v2/"+repo+"/manifests/b", nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE tag: status = %d, want 202", resp.StatusCode)
	}

	_, tags := tagsOf(t, do(t, server, http.MethodGet, "/v2/"+repo+"/tags/list?last=b", nil, nil))

	if want := []string{"c"}; !equal(tags, want) {
		t.Errorf("tags after a deleted cursor = %v, want %v", tags, want)
	}
}

func TestTagListPageBoundaries(t *testing.T) {
	server := newServer(t)

	for _, tag := range []string{"a", "b", "c"} {
		tagged(t, server, repo, tag)
	}

	cases := []struct {
		name     string
		query    string
		want     []string
		wantLink bool
	}{
		{"no n returns everything", "", []string{"a", "b", "c"}, false},
		{"n larger than the set", "?n=10", []string{"a", "b", "c"}, false},
		// Exactly the whole set: there is no next page, so a Link would send a
		// client on a round trip to learn nothing.
		{"n equal to the set", "?n=3", []string{"a", "b", "c"}, false},
		{"n smaller", "?n=2", []string{"a", "b"}, true},
		// The spec is explicit: an empty list and no Link.
		{"n zero", "?n=0", []string{}, false},
		{"last past the end", "?last=z", []string{}, false},
		{"n with last", "?n=1&last=a", []string{"b"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, server, http.MethodGet, "/v2/"+repo+"/tags/list"+tc.query, nil, nil)

			_, tags := tagsOf(t, resp)
			if !equal(tags, tc.want) {
				t.Errorf("tags = %v, want %v", tags, tc.want)
			}

			if link := resp.Header.Get("Link"); (link != "") != tc.wantLink {
				t.Errorf("Link = %q, want present = %v", link, tc.wantLink)
			}
		})
	}
}

func TestTagListRejectsBadParameters(t *testing.T) {
	server := newServer(t)

	tagged(t, server, repo, "v1")

	for _, query := range []string{"?n=abc", "?n=-1", "?last=not/a/tag"} {
		resp := do(t, server, http.MethodGet, "/v2/"+repo+"/tags/list"+query, nil, nil)

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, resp.StatusCode)
		}
	}
}

// TestTagListDistinguishesEmptyFromUnknown is the decision behind
// RepositoryExists: a typo must not look like a repository that has no tags.
func TestTagListDistinguishesEmptyFromUnknown(t *testing.T) {
	server := newServer(t)

	// A repository holding a blob but no tags exists, and answers with an empty
	// list. Pushing a blob is enough to bring it into being; there is no create.
	descriptorFor(t, server, repo, emptyConfigType, []byte("{}"))

	_, tags := tagsOf(t, do(t, server, http.MethodGet, "/v2/"+repo+"/tags/list", nil, nil))
	if len(tags) != 0 {
		t.Errorf("tags = %v, want empty", tags)
	}

	resp := do(t, server, http.MethodGet, "/v2/acme/nothing/tags/list", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown repository: status = %d, want 404", resp.StatusCode)
	}

	if code := errorCode(t, resp); code != "NAME_UNKNOWN" {
		t.Errorf("code = %s, want NAME_UNKNOWN", code)
	}
}

// TestEmptyTagListIsAnArray guards a detail clients have historically broken on:
// a nil slice marshals as null, and null is not a list.
func TestEmptyTagListIsAnArray(t *testing.T) {
	server := newServer(t)

	descriptorFor(t, server, repo, emptyConfigType, []byte("{}"))

	resp := do(t, server, http.MethodGet, "/v2/"+repo+"/tags/list", nil, nil)

	var body map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got := string(body["tags"]); got != "[]" {
		t.Errorf("tags = %s, want []", got)
	}
}

// TestDeleteTagKeepsTheManifest separates the two things end-9 can mean.
func TestDeleteTagKeepsTheManifest(t *testing.T) {
	server := newServer(t)

	dgst := tagged(t, server, repo, "v1")

	resp := do(t, server, http.MethodDelete, "/v2/"+repo+"/manifests/v1", nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	resp = do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/v1", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("tag still resolves: %d", resp.StatusCode)
	}

	// The manifest is still there. Only the name was withdrawn.
	resp = do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/"+dgst, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("manifest went with the tag: %d", resp.StatusCode)
	}
}

// TestDeleteManifestCascadesToTags is the inverse, and the reason the metastore
// runs a transaction: the spec requires every tag pointing at a deleted manifest
// to stop resolving, and a listing that advertised one would be worse than useless.
func TestDeleteManifestCascadesToTags(t *testing.T) {
	server := newServer(t)

	raw, dgst := imageManifest(t, server, repo)

	resp := do(t, server, http.MethodPut,
		fmt.Sprintf("/v2/%s/manifests/%s?tag=v1&tag=latest", repo, dgst),
		strings.NewReader(string(raw)),
		map[string]string{"Content-Type": imageManifestType})

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push: status = %d, want 201", resp.StatusCode)
	}

	resp = do(t, server, http.MethodDelete, "/v2/"+repo+"/manifests/"+dgst.String(), nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE: status = %d, want 202", resp.StatusCode)
	}

	// Neither name survives its referent.
	for _, tag := range []string{"v1", "latest"} {
		resp := do(t, server, http.MethodGet, "/v2/"+repo+"/manifests/"+tag, nil, nil)

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("tag %s still resolves after its manifest was deleted: %d",
				tag, resp.StatusCode)
		}
	}

	// And the listing does not advertise them either, which is the failure the
	// cascade exists to prevent.
	_, tags := tagsOf(t, do(t, server, http.MethodGet, "/v2/"+repo+"/tags/list", nil, nil))
	if len(tags) != 0 {
		t.Errorf("listing still advertises %v", tags)
	}
}

// TestTagsAreScopedToRepository covers the same isolation the blob and manifest
// endpoints have: a name is per repository, so two repositories may both call
// something "latest" and mean different things.
//
// The two manifests are given different configs on purpose. Identical bytes would
// produce one digest and the test would pass without proving anything.
func TestTagsAreScopedToRepository(t *testing.T) {
	server := newServer(t)

	const other = "acme/gadgets"

	mine := taggedVariant(t, server, repo, "latest", `{"who":"widgets"}`)
	theirs := taggedVariant(t, server, other, "latest", `{"who":"gadgets"}`)

	if mine == theirs {
		t.Fatal("test is not exercising isolation: both repositories hold the same digest")
	}

	for _, tc := range []struct{ repository, want string }{
		{repo, mine},
		{other, theirs},
	} {
		resp := do(t, server, http.MethodGet, "/v2/"+tc.repository+"/manifests/latest", nil, nil)

		if got := resp.Header.Get("Docker-Content-Digest"); got != tc.want {
			t.Errorf("latest in %s = %q, want %q", tc.repository, got, tc.want)
		}
	}

	// A tag set in one repository is not visible in another, even though both
	// manifests share the content store.
	_, tags := tagsOf(t, do(t, server, http.MethodGet, "/v2/"+other+"/tags/list", nil, nil))
	if want := []string{"latest"}; !equal(tags, want) {
		t.Errorf("tags in %s = %v, want %v", other, tags, want)
	}
}

// taggedVariant pushes a manifest whose config bytes are given, so callers can
// make two repositories hold genuinely different content.
func taggedVariant(t *testing.T, server *httptest.Server, repository, tag, config string) string {
	t.Helper()

	descriptor := descriptorFor(t, server, repository, emptyConfigType, []byte(config))

	raw, dgst := encode(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     imageManifestType,
		"config":        descriptor,
		"layers":        []any{},
	})

	resp := do(t, server, http.MethodPut,
		"/v2/"+repository+"/manifests/"+tag, strings.NewReader(string(raw)),
		map[string]string{"Content-Type": imageManifestType})

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT %s/%s: status = %d: %s",
			repository, tag, resp.StatusCode, errorCode(t, resp))
	}

	return dgst.String()
}

// TestRepositoryNamedTags covers the routing ambiguity, as the blob and manifest
// suites do for their own sections: "tags" is a legal name component.
func TestRepositoryNamedTags(t *testing.T) {
	server := newServer(t)

	const name = "acme/tags"

	tagged(t, server, name, "v1")

	got, tags := tagsOf(t, do(t, server, http.MethodGet, "/v2/"+name+"/tags/list", nil, nil))

	if got != name {
		t.Errorf("name = %q, want %q", got, name)
	}

	if want := []string{"v1"}; !equal(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
}

// TestRejectedTagNames pins the grammar. A tag becomes a URL path component and a
// storage key, so the anchoring matters.
func TestRejectedTagNames(t *testing.T) {
	server := newServer(t)

	raw, dgst := imageManifest(t, server, repo)

	// Via ?tag=, because the path form of an illegal tag would not route here at
	// all -- a slash makes it a different URL.
	for _, tag := range []string{".leading-dot", "-leading-hyphen", "has space", strings.Repeat("x", 129)} {
		query := url.Values{}
		query.Set("tag", tag)

		resp := do(t, server, http.MethodPut,
			fmt.Sprintf("/v2/%s/manifests/%s?%s", repo, dgst, query.Encode()),
			strings.NewReader(string(raw)),
			map[string]string{"Content-Type": imageManifestType})

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("tag %q: status = %d, want 400", tag, resp.StatusCode)
		}
	}
}

// nextFromLink extracts the URL from a rel="next" Link header, returning "" when
// there is none.
func nextFromLink(t *testing.T, header string) string {
	t.Helper()

	if header == "" {
		return ""
	}

	if !strings.Contains(header, `rel="next"`) {
		t.Fatalf("Link %q is not a next relation", header)
	}

	start := strings.Index(header, "<")
	end := strings.Index(header, ">")

	if start < 0 || end < start {
		t.Fatalf("Link %q is not bracketed", header)
	}

	return header[start+1 : end]
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}

	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}

	return true
}
