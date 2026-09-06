// Package uploadstore holds the bytes of in-progress uploads.
//
// It is deliberately separate from the blob store. A blob is named by the hash
// of its own content and is therefore complete and immutable by construction; a
// partial upload is neither, has no digest yet, and may never acquire one. Keeping
// the two in one namespace would mean the content-addressed store contains
// entries that are not addressed by their content.
package uploadstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
)

// ErrNotFound is returned when a session has no staged bytes.
var ErrNotFound = errors.New("upload not found")

// ErrInvalidID is returned when an identifier is not one this package issued.
var ErrInvalidID = errors.New("invalid upload id")

// Store stages the bytes of an upload until it is committed or abandoned.
type Store interface {
	Create(id string) error
	// Append writes r to the end of the session and returns the new total.
	Append(id string, r io.Reader) (int64, error)
	Size(id string) (int64, error)
	// Digest hashes the staged bytes without consuming them.
	Digest(id string) (digest.Digest, error)
	Open(id string) (io.ReadCloser, error)
	Discard(id string) error
}

// FS stages uploads as files under a directory.
type FS struct {
	root string
}

// NewFS opens or creates an upload area rooted at dir.
func NewFS(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create upload root: %w", err)
	}

	return &FS{root: dir}, nil
}

func (f *FS) Create(id string) error {
	path, err := f.path(id)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create upload: %w", err)
	}

	return file.Close()
}

func (f *FS) Append(id string, r io.Reader) (int64, error) {
	path, err := f.path(id)
	if err != nil {
		return 0, err
	}

	// O_APPEND without O_CREATE: appending to a session that does not exist is a
	// distinct error, not a silent new session.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrNotFound
		}

		return 0, fmt.Errorf("open upload: %w", err)
	}

	defer file.Close()

	if _, err := io.Copy(file, r); err != nil {
		return 0, fmt.Errorf("append to upload: %w", err)
	}

	// Durability before the offset is reported. A client told that bytes 0-N are
	// held will never send them again, so losing them to a crash would strand
	// the session with no way for either side to notice.
	if err := file.Sync(); err != nil {
		return 0, fmt.Errorf("sync upload: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat upload: %w", err)
	}

	return info.Size(), nil
}

func (f *FS) Size(id string) (int64, error) {
	path, err := f.path(id)
	if err != nil {
		return 0, err
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrNotFound
		}

		return 0, fmt.Errorf("stat upload: %w", err)
	}

	return info.Size(), nil
}

// Digest hashes the staged bytes.
//
// This exists so the digest a client claims can be checked *before* anything is
// promoted into the blob store. Verifying afterwards would mean either trusting
// the claim briefly or deleting a blob at its true digest to undo the mistake --
// and that digest may be one another repository legitimately references.
func (f *FS) Digest(id string) (digest.Digest, error) {
	reader, err := f.Open(id)
	if err != nil {
		return "", err
	}

	defer reader.Close()

	verifier := digest.Canonical.Digester()

	if _, err := io.Copy(verifier.Hash(), reader); err != nil {
		return "", fmt.Errorf("hash upload: %w", err)
	}

	return verifier.Digest(), nil
}

func (f *FS) Open(id string) (io.ReadCloser, error) {
	path, err := f.path(id)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}

		return nil, fmt.Errorf("open upload: %w", err)
	}

	return file, nil
}

func (f *FS) Discard(id string) error {
	path, err := f.path(id)
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}

		return fmt.Errorf("discard upload: %w", err)
	}

	return nil
}

// path resolves a session identifier to a file.
//
// The identifier is the only part of an upload request that reaches the
// filesystem, so it is re-validated here at the point of use rather than trusted
// to have been checked by the caller. Rejecting anything but lower-case hex
// leaves no room for "..", "/" or a NUL byte to appear in the path.
func (f *FS) path(id string) (string, error) {
	if !ValidID(id) {
		return "", ErrInvalidID
	}

	return filepath.Join(f.root, id), nil
}

// idLength is the hex length of an identifier, so 16 random bytes.
const idLength = 32

// NewID returns an unguessable session identifier.
//
// The session URL is the only thing standing between an in-progress upload and
// anyone else who can reach the registry: the spec has no notion of an upload
// belonging to a client beyond possession of its location. A sequential or
// otherwise predictable identifier would let a third party append bytes to
// somebody else's upload, so this comes from a CSPRNG.
func NewID() (string, error) {
	var raw [idLength / 2]byte

	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate upload id: %w", err)
	}

	return hex.EncodeToString(raw[:]), nil
}

// ValidID reports whether id has the form NewID produces.
func ValidID(id string) bool {
	if len(id) != idLength {
		return false
	}

	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}
