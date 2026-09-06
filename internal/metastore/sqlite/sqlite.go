// Package sqlite implements metastore.Store on SQLite.
//
// Nothing here is SQLite-specific beyond the DSN and the pragmas; the same
// schema and queries run on Postgres with placeholder renumbering.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/tkircsi/cairn/internal/metastore"
	"github.com/tkircsi/cairn/internal/model"

	_ "modernc.org/sqlite" // pure-Go driver, so no cgo
)

const schema = `
CREATE TABLE IF NOT EXISTS blobs (
    repository TEXT    NOT NULL,
    digest     TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    created_at TEXT    NOT NULL,
    PRIMARY KEY (repository, digest)
) WITHOUT ROWID;

-- The primary key answers "does this repository hold this blob", which is the
-- read path. This index answers "does anyone hold it", which is what a
-- cross-repository mount asks and what a garbage collector would need.
CREATE INDEX IF NOT EXISTS blobs_by_digest ON blobs (digest);

CREATE TABLE IF NOT EXISTS uploads (
    id         TEXT    NOT NULL PRIMARY KEY,
    repository TEXT    NOT NULL,
    received   INTEGER NOT NULL DEFAULT 0,
    started_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
) WITHOUT ROWID;

-- Sessions are the only rows that expire. Ordering by age is how a sweeper finds
-- the abandoned ones, which otherwise accumulate staged bytes forever.
CREATE INDEX IF NOT EXISTS uploads_by_updated_at ON uploads (updated_at);
`

// Store is a SQLite-backed metastore.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at dsn.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()

			return nil, fmt.Errorf("apply %q: %w", pragma, err)
		}
	}

	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()

		return nil, fmt.Errorf("apply schema: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) PutBlob(ctx context.Context, b model.Blob) error {
	const query = `
INSERT INTO blobs (repository, digest, size, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (repository, digest) DO NOTHING`

	_, err := s.db.ExecContext(ctx, query,
		b.Repository, b.Digest.String(), b.Size, formatTime(b.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert blob: %w", err)
	}

	return nil
}

func (s *Store) Blob(ctx context.Context, repository string, dgst digest.Digest) (model.Blob, error) {
	const query = `
SELECT repository, digest, size, created_at
FROM blobs
WHERE repository = ? AND digest = ?`

	return scanBlob(s.db.QueryRowContext(ctx, query, repository, dgst.String()))
}

func (s *Store) AnyBlob(ctx context.Context, dgst digest.Digest) (model.Blob, error) {
	// LIMIT 1 because the question is existence, not enumeration: the bytes are
	// identical in every repository that holds them, so the first row answers it.
	const query = `
SELECT repository, digest, size, created_at
FROM blobs
WHERE digest = ?
LIMIT 1`

	return scanBlob(s.db.QueryRowContext(ctx, query, dgst.String()))
}

func (s *Store) DeleteBlob(ctx context.Context, repository string, dgst digest.Digest) error {
	// Only the membership row goes. The bytes stay, because another repository
	// may hold the same digest and this store has no way to know it is the last
	// reference without a sweep it does not perform.
	const query = `DELETE FROM blobs WHERE repository = ? AND digest = ?`

	return s.execExpectingRow(ctx, query, repository, dgst.String())
}

func (s *Store) CreateUpload(ctx context.Context, u model.Upload) error {
	const query = `
INSERT INTO uploads (id, repository, received, started_at, updated_at)
VALUES (?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, query,
		u.ID, u.Repository, u.Received, formatTime(u.StartedAt), formatTime(u.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert upload: %w", err)
	}

	return nil
}

func (s *Store) Upload(ctx context.Context, id string) (model.Upload, error) {
	const query = `
SELECT id, repository, received, started_at, updated_at
FROM uploads
WHERE id = ?`

	var (
		u         model.Upload
		startedAt string
		updatedAt string
	)

	err := s.db.QueryRowContext(ctx, query, id).
		Scan(&u.ID, &u.Repository, &u.Received, &startedAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Upload{}, metastore.ErrNotFound
	}

	if err != nil {
		return model.Upload{}, fmt.Errorf("scan upload: %w", err)
	}

	if u.StartedAt, err = parseTime(startedAt); err != nil {
		return model.Upload{}, err
	}

	if u.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return model.Upload{}, err
	}

	return u, nil
}

func (s *Store) SetUploadReceived(ctx context.Context, id string, received int64, at time.Time) error {
	const query = `UPDATE uploads SET received = ?, updated_at = ? WHERE id = ?`

	return s.execExpectingRow(ctx, query, received, formatTime(at), id)
}

func (s *Store) DeleteUpload(ctx context.Context, id string) error {
	const query = `DELETE FROM uploads WHERE id = ?`

	return s.execExpectingRow(ctx, query, id)
}

// execExpectingRow runs a statement that must affect exactly one row, reporting
// ErrNotFound when it affects none. Without this, deleting a session that does
// not exist would look like success and the caller could not answer
// BLOB_UPLOAD_UNKNOWN.
func (s *Store) execExpectingRow(ctx context.Context, query string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if affected == 0 {
		return metastore.ErrNotFound
	}

	return nil
}

func scanBlob(row *sql.Row) (model.Blob, error) {
	var (
		b         model.Blob
		dgst      string
		createdAt string
	)

	err := row.Scan(&b.Repository, &dgst, &b.Size, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Blob{}, metastore.ErrNotFound
	}

	if err != nil {
		return model.Blob{}, fmt.Errorf("scan blob: %w", err)
	}

	b.Digest = digest.Digest(dgst)

	if b.CreatedAt, err = parseTime(createdAt); err != nil {
		return model.Blob{}, err
	}

	return b, nil
}

// Times are stored as UTC RFC3339 text: it sorts lexically in the same order it
// sorts chronologically, so an index on a timestamp column is usable directly.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}

	return t, nil
}
