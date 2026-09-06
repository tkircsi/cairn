// Package model holds what the store records about content.
//
// Neither type carries bytes. The bytes live in the blob store, addressed by
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
