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
	// Put writes data and returns its digest. The digest is always derived from
	// the bytes, never accepted from the caller.
	Put(data []byte) (digest.Digest, error)
	// Get returns the bytes for dgst, verifying them against it.
	Get(dgst digest.Digest) ([]byte, error)
	Stat(dgst digest.Digest) (int64, error)
	Delete(dgst digest.Digest) error
}

// FS is a filesystem-backed Store.
type FS struct {
	root string
}

// NewFS opens or creates a blob store rooted at dir.
func NewFS(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create blob root: %w", err)
	}

	return &FS{root: dir}, nil
}

func (f *FS) Put(data []byte) (digest.Digest, error) {
	dgst := digest.FromBytes(data)

	path := f.path(dgst)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create blob dir: %w", err)
	}

	// Already present: content-addressed, so there is nothing to update.
	if _, err := os.Stat(path); err == nil {
		return dgst, nil
	}

	// Write to a temporary name and rename, so a reader never observes a
	// partially written blob at a digest that promises complete content.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp blob: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()

		return "", fmt.Errorf("write temp blob: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp blob: %w", err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("commit blob: %w", err)
	}

	return dgst, nil
}

func (f *FS) Get(dgst digest.Digest) ([]byte, error) {
	data, err := os.ReadFile(f.path(dgst))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}

		return nil, fmt.Errorf("read blob: %w", err)
	}

	// Verify on read. A registry backed by a mutable tag cannot do this; here it
	// is a hash of bytes already in memory, so it is close to free and it turns
	// tampering into an error instead of a successful wrong answer.
	if actual := digest.FromBytes(data); actual != dgst {
		return nil, fmt.Errorf("integrity failure: stored bytes hash to %s, not %s", actual, dgst)
	}

	return data, nil
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

// Open exposes the blob as a ReadSeeker so an HTTP handler can hand it to
// http.ServeContent and get Range, If-Range and 416 handling for free.
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

func (f *FS) path(dgst digest.Digest) string {
	hex := dgst.Encoded()

	return filepath.Join(f.root, dgst.Algorithm().String(), hex[:2], hex)
}
