// Package model holds the stored shapes. These are the only types the storage
// layers agree on; protobuf and HTTP types do not leak into them.
package model

import (
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Manifest is an OCI manifest or index as stored.
//
// There is no separate "referrer" type: per the image spec a referrer is simply
// a manifest whose Subject names another manifest. That is why the referrers
// query is one indexed lookup on a column rather than a document to maintain.
type Manifest struct {
	Repository string
	Digest     digest.Digest
	MediaType  string

	// ArtifactType as it must appear on a referrers descriptor. The spec's
	// fallback rule (empty on an image manifest means the config descriptor's
	// mediaType; empty on an index means omit) is resolved once at ingest so
	// that reads stay a plain indexed filter.
	ArtifactType string

	// Subject is empty for a manifest that refers to nothing.
	Subject     digest.Digest
	Size        int64
	Annotations map[string]string
	CreatedAt   time.Time
}

// Descriptor renders the manifest as an OCI descriptor. This is the only place
// the stored model meets the wire format, so the referrers endpoint is a query
// plus a map over this method.
func (m Manifest) Descriptor() ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType:    m.MediaType,
		Digest:       m.Digest,
		Size:         m.Size,
		ArtifactType: m.ArtifactType,
		Annotations:  m.Annotations,
	}
}

// Blob is a non-manifest blob (a layer or a config) known to a repository. The
// bytes live in the blob store; this row only records membership and size.
type Blob struct {
	Repository string
	Digest     digest.Digest
	Size       int64
	CreatedAt  time.Time
}
