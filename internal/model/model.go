// Package model holds what the store records about content.
//
// No type here carries bytes. The bytes live in the blob store, addressed by
// their own digest; these are the facts about them that a digest cannot state.
package model

import (
	"time"

	"github.com/opencontainers/go-digest"
)

// Blob records that a repository contains a blob.
//
// Note what this row is *for*. The bytes are content-addressed and therefore
// shared by every repository that pushes them, but the spec scopes blob reads
// to a repository: the same bytes must read as 404 in a repository that never
// pushed them. That membership fact is the one thing the digest cannot express,
// and it is the reason a content-addressed store still needs an index.
type Blob struct {
	Repository string
	Digest     digest.Digest
	Size       int64
	CreatedAt  time.Time
}

// Upload is an in-progress blob upload.
//
// One of the two kinds of mutable state here, the other being a Tag. Content is
// named by the digest of its own bytes and so can never change; a session exists
// precisely because the client is not required to know that digest until it
// closes.
type Upload struct {
	// ID is opaque to clients and is the only part of a request that becomes a
	// filesystem path, so it is generated here rather than accepted.
	ID         string
	Repository string
	// Received is the number of bytes accepted so far, which is also the offset
	// the next chunk must start at.
	Received  int64
	StartedAt time.Time
	UpdatedAt time.Time
}

// Manifest records a manifest a repository holds.
//
// A manifest's bytes are content-addressed like any other content and live in
// the blob store. What is worth indexing is everything a client needs to *find*
// or *describe* one without parsing it: a GET has to answer with the right
// Content-Type, and a referrers query has to select on a pointer buried inside
// the document.
//
// The bytes are deliberately not registered as a blob of the repository, so a
// manifest digest is a 404 on the blob endpoints. The two namespaces share
// storage, not addressing.
type Manifest struct {
	Repository string
	Digest     digest.Digest
	// MediaType is what the manifest says it is, and is echoed back as the
	// Content-Type of a GET. Without it stored, serving a manifest would mean
	// reading and parsing it first.
	MediaType string
	// ArtifactType is the OCI 1.1 field describing what an artifact *is*, as
	// distinct from how it is encoded. It is the filter a referrers query
	// applies, which is why it is a column and not left inside the bytes.
	//
	// This holds the *effective* type rather than verbatim what the document said.
	// The spec defines a fallback -- an image manifest without one is described by
	// its config's media type, an index without one has none -- and resolving it
	// once at write time is what lets the referrers filter be a column comparison
	// instead of a rule reapplied per row on every query.
	ArtifactType string
	// Subject is the manifest this one makes a statement about -- a signature,
	// an SBOM, an attestation -- and is empty for a manifest that stands alone.
	//
	// This is the field that turns the index from a convenience into the point.
	// The pointer is stored on the child, but every client wants to query it from
	// the parent: "what refers to this?". Answering that by scanning manifests is
	// O(N); answering it from an index on this column is a seek.
	Subject digest.Digest
	// Annotations are the manifest's own annotations, which a referrers response
	// has to reproduce: they are how a client tells two signatures apart without
	// fetching either, so leaving them in the bytes would make listing referrers a
	// read of every one of them.
	//
	// Nil when the manifest has none, which is the common case.
	Annotations map[string]string
	Size        int64
	CreatedAt   time.Time
}

// Tag is a human-chosen name for a manifest.
//
// The one piece of naming in the store that a person picks and that can be moved
// later, which makes it the only place a client can ask for content without
// already knowing its digest. That is the whole reason it exists: "give me
// v1.2.3" is answerable, "give me sha256:47cf..." requires having been told.
//
// Being mutable is also why it is a row of its own rather than a column on
// Manifest. A manifest is immutable and may carry many tags; a tag points at
// exactly one manifest and is expected to be repointed at another.
type Tag struct {
	Repository string
	// Name is the tag as written by the client, matched byte for byte. Two tags
	// differing only in case are two tags, which is what the listing order the
	// spec requires implies.
	Name   string
	Digest digest.Digest
	// UpdatedAt is when the tag last moved, not when the manifest was pushed.
	// Moving a tag is the only write in this store that destroys information, so
	// it is the one worth timestamping.
	UpdatedAt time.Time
}
