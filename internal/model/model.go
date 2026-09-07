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
// This is the only mutable state in the store. Everything else is named by a
// digest of its own content and so can never change; a session exists precisely
// because the client is not required to know the digest until it closes.
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
	ArtifactType string
	// Subject is the manifest this one makes a statement about -- a signature,
	// an SBOM, an attestation -- and is empty for a manifest that stands alone.
	//
	// This is the field that turns the index from a convenience into the point.
	// The pointer is stored on the child, but every client wants to query it from
	// the parent: "what refers to this?". Answering that by scanning manifests is
	// O(N); answering it from an index on this column is a seek.
	Subject   digest.Digest
	Size      int64
	CreatedAt time.Time
}
