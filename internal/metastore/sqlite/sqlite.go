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

CREATE TABLE IF NOT EXISTS manifests (
    repository    TEXT    NOT NULL,
    digest        TEXT    NOT NULL,
    media_type    TEXT    NOT NULL,
    -- Empty string rather than NULL, so scanning needs no sql.NullString and the
    -- Go zero value round-trips. digest.Digest("") is already the natural "none".
    artifact_type TEXT    NOT NULL DEFAULT '',
    subject       TEXT    NOT NULL DEFAULT '',
    size          INTEGER NOT NULL,
    created_at    TEXT    NOT NULL,
    PRIMARY KEY (repository, digest)
) WITHOUT ROWID;

-- The primary key is clustered on (repository, digest), so it cannot answer a
-- query on digest alone -- and "does anything still reference these bytes" is
-- exactly that query. Without this index a garbage collector's liveness check
-- degrades into a full scan of the table per file it considers.
CREATE INDEX IF NOT EXISTS manifests_by_digest ON manifests (digest);

-- No index on subject yet, deliberately. It would serve only the referrers
-- endpoint, which does not exist, and until then it is write amplification on
-- every push. The asymmetry that decides this: CREATE INDEX IF NOT EXISTS is
-- free to add later because this schema is reapplied on open, whereas adding a
-- *column* later would need an ALTER and a backfill that nothing here performs.
-- So the columns land now and the index waits.

CREATE TABLE IF NOT EXISTS tags (
    repository TEXT NOT NULL,
    name       TEXT NOT NULL,
    digest     TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    -- Clustered on (repository, name), which is exactly the order end-8a must
    -- return tags in: listing a repository is an ordered range scan of the
    -- primary key with no sort step, and paginating with "last" is a seek into
    -- the middle of it.
    --
    -- The default BINARY collation is load-bearing rather than incidental. It
    -- compares byte by byte, which for UTF-8 is Go's sort.Strings -- the exact
    -- order the spec names. COLLATE NOCASE here would silently produce a
    -- different order than clients paginating with "last" expect, and the
    -- disagreement would only show up as skipped or repeated tags near a page
    -- boundary.
    PRIMARY KEY (repository, name)
) WITHOUT ROWID;

-- Deleting a manifest has to delete the tags pointing at it, and that is a query
-- on digest, which the primary key cannot answer.
CREATE INDEX IF NOT EXISTS tags_by_digest ON tags (repository, digest);

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

func (s *Store) PutManifest(ctx context.Context, m model.Manifest) error {
	const query = `
INSERT INTO manifests (repository, digest, media_type, artifact_type, subject, size, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repository, digest) DO NOTHING`

	_, err := s.db.ExecContext(ctx, query,
		m.Repository, m.Digest.String(), m.MediaType, m.ArtifactType,
		m.Subject.String(), m.Size, formatTime(m.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert manifest: %w", err)
	}

	return nil
}

func (s *Store) Manifest(
	ctx context.Context,
	repository string,
	dgst digest.Digest,
) (model.Manifest, error) {
	const query = `
SELECT repository, digest, media_type, artifact_type, subject, size, created_at
FROM manifests
WHERE repository = ? AND digest = ?`

	var (
		m         model.Manifest
		dgstText  string
		subject   string
		createdAt string
	)

	err := s.db.QueryRowContext(ctx, query, repository, dgst.String()).Scan(
		&m.Repository, &dgstText, &m.MediaType, &m.ArtifactType,
		&subject, &m.Size, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Manifest{}, metastore.ErrNotFound
	}

	if err != nil {
		return model.Manifest{}, fmt.Errorf("scan manifest: %w", err)
	}

	m.Digest = digest.Digest(dgstText)
	m.Subject = digest.Digest(subject)

	if m.CreatedAt, err = parseTime(createdAt); err != nil {
		return model.Manifest{}, err
	}

	return m, nil
}

// DeleteManifest removes a manifest and every tag in the repository pointing at
// it, in one transaction.
//
// The cascade is required, not tidiness: the spec says that once a manifest is
// deleted, a GET to any tag pointing at that digest returns 404. Leaving the tag
// rows would satisfy that by accident -- resolution goes through the manifest,
// which is gone -- but end-8a would then advertise tags that cannot be fetched,
// which is worse than not listing them.
//
// This is the only transaction in the store, and it is here because the
// intermediate state is externally visible. Between the two statements a
// concurrent GET could resolve a tag to a manifest that is being deleted, and
// SQLite's atomicity is what makes that unobservable.
func (s *Store) DeleteManifest(ctx context.Context, repository string, dgst digest.Digest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	// Rollback after a successful Commit is a no-op, so this needs no flag.
	defer tx.Rollback() //nolint:errcheck // committed path already returned

	const dropTags = `DELETE FROM tags WHERE repository = ? AND digest = ?`

	if _, err := tx.ExecContext(ctx, dropTags, repository, dgst.String()); err != nil {
		return fmt.Errorf("delete tags for manifest: %w", err)
	}

	// As with a blob, only the row goes: the bytes may be referenced by an index
	// in another repository, and nothing here decides last-reference.
	const dropManifest = `DELETE FROM manifests WHERE repository = ? AND digest = ?`

	result, err := tx.ExecContext(ctx, dropManifest, repository, dgst.String())
	if err != nil {
		return fmt.Errorf("delete manifest: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	// Rolling back rather than committing the tag deletes: if the manifest was not
	// there, the caller gets a 404 and must not also have had side effects.
	if affected == 0 {
		return metastore.ErrNotFound
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	return nil
}

func (s *Store) PutTag(ctx context.Context, t model.Tag) error {
	// Upsert rather than insert: repointing a tag at a new manifest is the normal
	// way a release moves, so a conflict here is expected traffic and not an error.
	const query = `
INSERT INTO tags (repository, name, digest, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (repository, name) DO UPDATE SET
    digest     = excluded.digest,
    updated_at = excluded.updated_at`

	_, err := s.db.ExecContext(ctx, query,
		t.Repository, t.Name, t.Digest.String(), formatTime(t.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert tag: %w", err)
	}

	return nil
}

func (s *Store) Tag(ctx context.Context, repository, name string) (model.Tag, error) {
	const query = `
SELECT repository, name, digest, updated_at
FROM tags
WHERE repository = ? AND name = ?`

	var (
		t         model.Tag
		dgstText  string
		updatedAt string
	)

	err := s.db.QueryRowContext(ctx, query, repository, name).Scan(
		&t.Repository, &t.Name, &dgstText, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Tag{}, metastore.ErrNotFound
	}

	if err != nil {
		return model.Tag{}, fmt.Errorf("scan tag: %w", err)
	}

	t.Digest = digest.Digest(dgstText)

	if t.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return model.Tag{}, err
	}

	return t, nil
}

// Tags lists a repository's tag names in the order the spec requires.
//
// after is a cursor, not a filter: results begin strictly after it, and it need
// not be a tag that exists -- a client may hold a name that has since been
// deleted, and pagination has to survive that.
//
// A negative limit means unlimited; zero means no rows, which SQLite's LIMIT
// already expresses exactly, so neither case needs special handling below.
func (s *Store) Tags(ctx context.Context, repository, after string, limit int) ([]string, error) {
	// name > ? rather than an offset. An offset would renumber under a concurrent
	// push and silently skip a tag; a keyset cursor cannot, because it names a
	// position in the ordering rather than a count into it.
	//
	// ORDER BY name relies on the primary key's BINARY collation, which is the
	// ASCIIbetical order the spec asks for. LIMIT -1 is SQLite for unlimited and
	// LIMIT 0 for none, which is why the caller's convention is the same.
	const query = `
SELECT name
FROM tags
WHERE repository = ? AND name > ?
ORDER BY name
LIMIT ?`

	if limit < 0 {
		limit = -1
	}

	rows, err := s.db.QueryContext(ctx, query, repository, after, limit)
	if err != nil {
		return nil, fmt.Errorf("query tags: %w", err)
	}

	defer rows.Close()

	// Non-nil so that an empty result marshals as [] rather than null, which the
	// response body for end-8a shows as a list.
	names := []string{}

	for rows.Next() {
		var name string

		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}

		names = append(names, name)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tags: %w", err)
	}

	return names, nil
}

func (s *Store) DeleteTag(ctx context.Context, repository, name string) error {
	// Only the name goes. The manifest it pointed at is untouched and still
	// reachable by digest, which is what distinguishes end-9 on a tag from end-9 on
	// a digest.
	const query = `DELETE FROM tags WHERE repository = ? AND name = ?`

	return s.execExpectingRow(ctx, query, repository, name)
}

// RepositoryExists reports whether anything at all is filed under a name.
//
// There is no repositories table, and deliberately so: a repository is not a
// thing a client creates, it is the scope a push happens to name. So existence is
// derived, and the derivation has to consider every table, or a repository
// holding only a staged upload would read as absent.
func (s *Store) RepositoryExists(ctx context.Context, repository string) (bool, error) {
	// No LIMIT on the arms: SQLite only accepts one at the end of a compound
	// SELECT, and it would be redundant anyway, because EXISTS stops at the first
	// row the compound produces.
	//
	// The first three arms are index seeks on a clustered primary key. The uploads
	// arm is a scan, since nothing indexes uploads by repository -- acceptable only
	// because that table holds in-flight sessions rather than history, so it is
	// small by construction.
	const query = `
SELECT EXISTS (
    SELECT 1 FROM blobs     WHERE repository = ?
    UNION ALL
    SELECT 1 FROM manifests WHERE repository = ?
    UNION ALL
    SELECT 1 FROM tags      WHERE repository = ?
    UNION ALL
    SELECT 1 FROM uploads   WHERE repository = ?
)`

	var exists bool

	err := s.db.QueryRowContext(ctx, query,
		repository, repository, repository, repository).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check repository %s: %w", repository, err)
	}

	return exists, nil
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

func (s *Store) IsReferenced(ctx context.Context, dgst digest.Digest) (bool, error) {
	// Both halves are index seeks: blobs_by_digest and manifests_by_digest exist for
	// this query, since neither primary key is usable on digest alone.
	//
	// UNION ALL rather than UNION: EXISTS stops at the first row, so deduplicating
	// the two sides would be work whose result is discarded.
	//
	// Tags are deliberately not consulted. A tag names a manifest rather than
	// bytes, and DeleteManifest drops the tags pointing at it in the same
	// transaction, so a tag can never be the last thing keeping content alive.
	const query = `
SELECT EXISTS (
    SELECT 1 FROM blobs     WHERE digest = ?
    UNION ALL
    SELECT 1 FROM manifests WHERE digest = ?
)`

	var referenced bool

	text := dgst.String()

	if err := s.db.QueryRowContext(ctx, query, text, text).Scan(&referenced); err != nil {
		return false, fmt.Errorf("check references to %s: %w", dgst, err)
	}

	return referenced, nil
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
