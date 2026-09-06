// Package blobstore stores bytes addressed by digest.
//
// The layout borrows Distribution's two-character fanout
// (blobs/sha256/<aa>/<hex>) purely because it keeps directories small; nothing
// reads meaning out of the path. Swapping this for S3 or a database column means
// implementing Store and changing nothing else.
package blobstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
)

// ErrNotFound is returned when a digest is not present.
var ErrNotFound = errors.New("blob not found")

// Store holds content-addressed bytes.
type Store interface {
	// Put streams r into the store and returns the digest of what was written.
	// The digest is always derived from the bytes, never accepted from the
	// caller, so a Store cannot be made to lie about what it holds.
	Put(r io.Reader) (digest.Digest, int64, error)
	// Get returns the bytes for dgst, verified against it.
	Get(dgst digest.Digest) ([]byte, error)
	// Open returns the blob as a ReadSeeker so a handler can hand it to
	// http.ServeContent and get Range, If-Range and 416 handling for free.
	Open(dgst digest.Digest) (io.ReadSeekCloser, error)
	Stat(dgst digest.Digest) (int64, error)
	Delete(dgst digest.Digest) error
}

// FS is a filesystem-backed Store.
type FS struct {
	root string
}

// NewFS opens or creates a blob store rooted at dir.
func NewFS(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create blob root: %w", err)
	}

	return &FS{root: dir}, nil
}

// Put streams r to a temporary file, hashing as it goes, then renames it into
// place under the digest of what was actually read.
//
// Streaming rather than buffering matters: a blob is arbitrarily large, and
// holding one in memory makes the process's footprint a function of what
// clients choose to upload.
func (f *FS) Put(r io.Reader) (digest.Digest, int64, error) {
	staging := filepath.Join(f.root, "incoming")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return "", 0, fmt.Errorf("create staging dir: %w", err)
	}

	tmp, err := os.CreateTemp(staging, "put-*")
	if err != nil {
		return "", 0, fmt.Errorf("create temp blob: %w", err)
	}

	defer os.Remove(tmp.Name())

	verifier := digest.Canonical.Digester()

	size, err := io.Copy(io.MultiWriter(tmp, verifier.Hash()), r)
	if err != nil {
		tmp.Close()

		return "", 0, fmt.Errorf("write temp blob: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("close temp blob: %w", err)
	}

	dgst := verifier.Digest()

	path := f.path(dgst)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", 0, fmt.Errorf("create blob dir: %w", err)
	}

	// Already present: content-addressed, so there is nothing to update and
	// re-linking identical bytes would only risk truncating a good blob.
	if _, err := os.Stat(path); err == nil {
		return dgst, size, nil
	}

	// Rename rather than write in place, so a reader never observes a partial
	// blob at a digest that promises complete content.
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", 0, fmt.Errorf("commit blob: %w", err)
	}

	return dgst, size, nil
}

func (f *FS) Get(dgst digest.Digest) ([]byte, error) {
	data, err := os.ReadFile(f.path(dgst))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}

		return nil, fmt.Errorf("read blob: %w", err)
	}

	// Verify on read. A registry keyed by a mutable tag cannot do this; here it
	// is a hash of bytes already in memory, so it is close to free and it turns
	// tampering into an error instead of a successful wrong answer.
	if actual := digest.FromBytes(data); actual != dgst {
		return nil, fmt.Errorf("integrity failure: stored bytes hash to %s, not %s", actual, dgst)
	}

	return data, nil
}

// Open returns a handle to the blob.
//
// Unlike Get this cannot verify the digest, because doing so would mean reading
// the whole blob before answering -- which defeats the Range support that is the
// reason to stream in the first place. Integrity here rests on the store not
// being writable by anything but this process.
func (f *FS) Open(dgst digest.Digest) (io.ReadSeekCloser, error) {
	file, err := os.Open(f.path(dgst))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}

		return nil, fmt.Errorf("open blob: %w", err)
	}

	return file, nil
}

func (f *FS) Stat(dgst digest.Digest) (int64, error) {
	info, err := os.Stat(f.path(dgst))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrNotFound
		}

		return 0, fmt.Errorf("stat blob: %w", err)
	}

	return info.Size(), nil
}

func (f *FS) Delete(dgst digest.Digest) error {
	if err := os.Remove(f.path(dgst)); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}

		return fmt.Errorf("delete blob: %w", err)
	}

	return nil
}

// path builds the on-disk location from the parsed parts of the digest rather
// than from its string form. Algorithm() and Encoded() are constrained to
// [a-z0-9+._-] and hex respectively, so no caller-supplied text can escape the
// root even if validation upstream is ever loosened.
func (f *FS) path(dgst digest.Digest) string {
	hex := dgst.Encoded()

	return filepath.Join(f.root, dgst.Algorithm().String(), hex[:2], hex)
}
