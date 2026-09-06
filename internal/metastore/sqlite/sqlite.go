// Package sqlite implements metastore.Store on SQLite.
//
// Nothing here is SQLite-specific beyond the DSN and the pragmas; the same
// schema and queries run on Postgres with placeholder renumbering.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/tkircsi/cairn/internal/metastore"
	"github.com/tkircsi/cairn/internal/model"

	_ "modernc.org/sqlite" // pure-Go driver, so no cgo
)

const schema = `
CREATE TABLE IF NOT EXISTS manifests (
    repository    TEXT    NOT NULL,
    digest        TEXT    NOT NULL,
    media_type    TEXT    NOT NULL,
    artifact_type TEXT    NOT NULL DEFAULT '',
    subject       TEXT    NOT NULL DEFAULT '',
    size          INTEGER NOT NULL,
    annotations   TEXT    NOT NULL DEFAULT '{}',
    created_at    TEXT    NOT NULL,
    PRIMARY KEY (repository, digest)
) WITHOUT ROWID;

-- This index is the entire point of the project. It answers end-12a directly,
-- which is the query a path-addressed registry cannot answer and therefore
-- delegates to a client-maintained sha256-<subject> document.
--
-- Partial, because only manifests that carry a subject are referrers, and those
-- are a small minority of rows.
CREATE INDEX IF NOT EXISTS manifests_by_subject
    ON manifests (repository, subject, artifact_type, digest)
    WHERE subject <> '';

CREATE TABLE IF NOT EXISTS blobs (
    repository TEXT    NOT NULL,
    digest     TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    created_at TEXT    NOT NULL,
    PRIMARY KEY (repository, digest)
) WITHOUT ROWID;
`

// Note on what is deliberately absent: there is no foreign key from
// manifests.subject to manifests.digest. The spec requires a registry to accept
// a manifest whose subject names content that is not present -- the conformance
// suite pushes exactly that case -- so a foreign key here would fail
// conformance. Cascading a subject delete onto its referrers is therefore a
// policy this store *can* express but does not impose.

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

func (s *Store) PutManifest(ctx context.Context, m model.Manifest) error {
	annotations, err := json.Marshal(m.Annotations)
	if err != nil {
		return fmt.Errorf("encode annotations: %w", err)
	}

	// Manifests are immutable at a digest, so a repeat push has nothing to
	// change. This is what makes the write path idempotent.
	const query = `
INSERT INTO manifests (repository, digest, media_type, artifact_type, subject, size, annotations, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repository, digest) DO NOTHING`

	_, err = s.db.ExecContext(ctx, query,
		m.Repository, m.Digest.String(), m.MediaType, m.ArtifactType,
		m.Subject.String(), m.Size, string(annotations), m.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert manifest: %w", err)
	}

	return nil
}

func (s *Store) Manifest(ctx context.Context, repository string, dgst digest.Digest) (model.Manifest, error) {
	const query = `
SELECT digest, media_type, artifact_type, subject, size, annotations, created_at
FROM manifests
WHERE repository = ? AND digest = ?`

	row := s.db.QueryRowContext(ctx, query, repository, dgst.String())

	m, err := scanManifest(repository, row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Manifest{}, metastore.ErrNotFound
	}

	return m, err
}

func (s *Store) DeleteManifest(ctx context.Context, repository string, dgst digest.Digest) error {
	const query = `DELETE FROM manifests WHERE repository = ? AND digest = ?`

	result, err := s.db.ExecContext(ctx, query, repository, dgst.String())
	if err != nil {
		return fmt.Errorf("delete manifest: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete manifest: %w", err)
	}

	if affected == 0 {
		return metastore.ErrNotFound
	}

	return nil
}

func (s *Store) PutBlob(ctx context.Context, b model.Blob) error {
	const query = `
INSERT INTO blobs (repository, digest, size, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (repository, digest) DO NOTHING`

	_, err := s.db.ExecContext(ctx, query,
		b.Repository, b.Digest.String(), b.Size, b.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert blob: %w", err)
	}

	return nil
}

func (s *Store) Referrers(ctx context.Context, q metastore.ReferrersQuery) ([]model.Manifest, error) {
	// Conditions are assembled from a fixed set of clauses and every value is
	// bound, so no caller input reaches the SQL text.
	conditions := []string{"repository = ?", "subject = ?"}
	args := []any{q.Repository, q.Subject.String()}

	if q.ArtifactType != "" {
		conditions = append(conditions, "artifact_type = ?")
		args = append(args, q.ArtifactType)
	}

	if q.After != "" {
		conditions = append(conditions, "digest > ?")
		args = append(args, q.After.String())
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	args = append(args, limit)

	// Ordering by digest matches the index, so the page is read straight off it
	// with no sort, and keyset pagination makes the last page as cheap as the
	// first.
	query := `
SELECT digest, media_type, artifact_type, subject, size, annotations, created_at
FROM manifests
WHERE ` + strings.Join(conditions, " AND ") + `
ORDER BY digest
LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query referrers: %w", err)
	}
	defer rows.Close()

	var referrers []model.Manifest

	for rows.Next() {
		m, err := scanManifest(q.Repository, rows)
		if err != nil {
			return nil, err
		}

		referrers = append(referrers, m)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan referrers: %w", err)
	}

	return referrers, nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanManifest(repository string, src scanner) (model.Manifest, error) {
	var (
		m           model.Manifest
		dgst        string
		subject     string
		annotations string
		createdAt   string
	)

	if err := src.Scan(&dgst, &m.MediaType, &m.ArtifactType, &subject, &m.Size, &annotations, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Manifest{}, err
		}

		return model.Manifest{}, fmt.Errorf("scan manifest: %w", err)
	}

	if err := json.Unmarshal([]byte(annotations), &m.Annotations); err != nil {
		return model.Manifest{}, fmt.Errorf("decode annotations: %w", err)
	}

	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return model.Manifest{}, fmt.Errorf("parse created_at: %w", err)
	}

	m.Repository = repository
	m.Digest = digest.Digest(dgst)
	m.Subject = digest.Digest(subject)
	m.CreatedAt = created

	return m, nil
}
