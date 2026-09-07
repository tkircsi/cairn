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
	"net/url"
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
    -- The manifest's annotations as a JSON object, or '' for none. Stored rather
    -- than read back out of the bytes because a referrers response must reproduce
    -- them, and re-reading every candidate manifest to answer one query is the
    -- O(N) behaviour this schema exists to avoid.
    annotations   TEXT    NOT NULL DEFAULT '',
    size          INTEGER NOT NULL,
    created_at    TEXT    NOT NULL,
    PRIMARY KEY (repository, digest)
) WITHOUT ROWID;

-- The primary key is clustered on (repository, digest), so it cannot answer a
-- query on digest alone -- and "does anything still reference these bytes" is
-- exactly that query. Without this index a garbage collector's liveness check
-- degrades into a full scan of the table per file it considers.
CREATE INDEX IF NOT EXISTS manifests_by_digest ON manifests (digest);

-- The index the referrers endpoint runs on, and the reason SQL is here at all.
-- The pointer is stored on the child but every client asks from the parent --
-- "what refers to this?" -- which is a table scan without this and a seek with it.
--
-- Partial, because most manifests are not referrers. Restricting it to rows that
-- have a subject keeps it proportional to the number of signatures and SBOMs
-- rather than to everything ever pushed, and keeps a plain image push from paying
-- to maintain an entry it would never appear in.
-- created_at and digest are in the key, after the two columns the query fixes,
-- because they are what it orders by. Without them the seek is followed by a sort
-- of everything it found; with them the index is already in the requested order and
-- is simply walked backwards. Both directions are DESC in the query for exactly this
-- reason -- a mixed ORDER BY cannot be satisfied by one traversal of one index.
CREATE INDEX IF NOT EXISTS manifests_by_subject
    ON manifests (repository, subject, created_at, digest)
    WHERE subject != '';

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

// migrations bring a database created by an older build up to the schema above.
//
// CREATE TABLE IF NOT EXISTS silently does nothing when the table is already
// there, so a column added to it after the fact never reaches a database that
// already exists. Indexes need no entry here -- CREATE INDEX IF NOT EXISTS
// applies on every open -- and neither do new tables. Only columns.
//
// Every string is a literal, with no interpolation and no user input anywhere
// near it, which is what makes DDL that cannot be parameterized safe to run.
var migrations = []struct {
	// name identifies the migration in an error, not in the database. There is no
	// version table: each check asks the schema itself whether it has already been
	// applied, which cannot drift out of step with reality the way a recorded
	// version number can.
	name string
	// check counts matching columns; a non-zero result means already applied.
	check string
	apply string
}{
	{
		name:  "manifests.annotations",
		check: `SELECT COUNT(*) FROM pragma_table_info('manifests') WHERE name = 'annotations'`,
		apply: `ALTER TABLE manifests ADD COLUMN annotations TEXT NOT NULL DEFAULT ''`,
	},
}

// Store is a SQLite-backed metastore.
type Store struct {
	db *sql.DB
}

// dsn turns a filesystem path into a connection string carrying the pragmas.
//
// Built as a URI so the path is escaped rather than trusted to contain no character the
// query parser cares about: a temporary directory or a data root with a "?" in it would
// otherwise have part of its name read as settings.
func dsn(path string) string {
	query := make(url.Values, len(pragmas))
	for _, pragma := range pragmas {
		query.Add("_pragma", pragma)
	}

	uri := url.URL{Scheme: "file", Opaque: (&url.URL{Path: path}).EscapedPath()}
	uri.RawQuery = query.Encode()

	return uri.String()
}

// pragmas configure every connection, and are passed in the DSN rather than executed
// after opening.
//
// The distinction is not stylistic. database/sql is a pool, and most PRAGMAs configure a
// connection rather than the database -- journal_mode is recorded in the file and
// persists, but busy_timeout, foreign_keys and analysis_limit reset to their defaults on
// every new connection. Running them with db.Exec configures whichever connection the
// pool happened to hand over and no other, so the second connection has no busy timeout
// and no foreign key enforcement.
//
// Nothing sequential can see it. One caller at a time reuses the one configured
// connection, so a full test suite, the OCI conformance suite and a 3000-push benchmark
// all pass while the pool holds unconfigured connections it has never needed. The
// symptom only appears when two callers arrive together -- as they do on an ordinary
// `oras push`, which uploads a manifest's blobs concurrently -- and then it is a 500 with
// nothing to connect it back to here.
var pragmas = []string{
	// Wait for a competing writer rather than failing at once. Without this a
	// concurrent write returns SQLITE_BUSY immediately, which the HTTP layer can only
	// report as an internal error for a request that was entirely valid.
	"busy_timeout(5000)",
	// The schema's cascades are declared as foreign keys, and SQLite does not enforce
	// them unless asked, per connection. Off, deletes silently leave the rows they
	// promised to remove.
	"foreign_keys(1)",
	"journal_mode(WAL)",
	// Bounds how much of an index ANALYZE will read, so gathering statistics stays
	// proportional to nothing in particular rather than to the size of the database.
	// SQLite's own recommended value.
	"analysis_limit(400)",
}

// Open opens (creating if needed) the database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()

		return nil, fmt.Errorf("apply schema: %w", err)
	}

	if err := migrate(ctx, db); err != nil {
		db.Close()

		return nil, err
	}

	store := &Store{db: db}

	// Statistics are not a tuning nicety here; without them the referrers query gets
	// the wrong plan. SQLite's planner will only prefer manifests_by_subject to a
	// prefix seek of the primary key once it knows how selective the subject column
	// is, and with no sqlite_stat1 it guesses -- landing on a plan that reads every
	// manifest in the repository. The index exists and is simply not chosen.
	//
	// So this runs on open as well as close. On close it records the shape of the
	// database that was just written; on open it is what makes a database that has
	// never been closed cleanly, or was created by an older build, usable now.
	if err := store.Optimize(ctx); err != nil {
		db.Close()

		return nil, err
	}

	return store, nil
}

// Optimize refreshes the query planner's statistics.
//
// PRAGMA optimize is a no-op unless something has changed enough to be worth
// re-analysing, which is what makes it safe to call on every open and close rather
// than needing a schedule.
//
// It is exported because the schedule is the caller's problem and cannot be solved
// here. Statistics are only as current as the last call, so a long-running daemon
// that opens an empty database and then serves a million pushes will hold the
// statistics of an empty database for the whole run -- the plan degrades quietly as
// the data it was chosen for stops resembling the data it runs against.
func (s *Store) Optimize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA optimize"); err != nil {
		return fmt.Errorf("optimize: %w", err)
	}

	return nil
}

// migrate applies any column additions the database is missing.
//
// Each is guarded by asking the schema rather than by tracking a version, so
// running this against a fresh database -- where the CREATE TABLE above already
// included the column -- is a no-op rather than an error.
func migrate(ctx context.Context, db *sql.DB) error {
	for _, m := range migrations {
		var present int
		if err := db.QueryRowContext(ctx, m.check).Scan(&present); err != nil {
			return fmt.Errorf("check migration %s: %w", m.name, err)
		}

		if present > 0 {
			continue
		}

		if _, err := db.ExecContext(ctx, m.apply); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
	}

	return nil
}

// Close records what the planner learned during this run and then closes.
//
// A failed optimize is not worth failing a shutdown over -- the database is intact
// either way and the next open will try again -- but it must not skip the close, so
// the error is dropped rather than returned.
func (s *Store) Close() error {
	_ = s.Optimize(context.Background())

	return s.db.Close()
}

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
INSERT INTO manifests (
    repository, digest, media_type, artifact_type, subject, annotations, size, created_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repository, digest) DO NOTHING`

	annotations, err := encodeAnnotations(m.Annotations)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, query,
		m.Repository, m.Digest.String(), m.MediaType, m.ArtifactType,
		m.Subject.String(), annotations, m.Size, formatTime(m.CreatedAt),
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
SELECT repository, digest, media_type, artifact_type, subject, annotations, size, created_at
FROM manifests
WHERE repository = ? AND digest = ?`

	m, err := scanManifest(s.db.QueryRowContext(ctx, query, repository, dgst.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Manifest{}, metastore.ErrNotFound
	}

	if err != nil {
		return model.Manifest{}, err
	}

	return m, nil
}

// referrersQuery is the statement Referrers runs.
//
// A package-level constant rather than a local one so that a test can put it
// through EXPLAIN QUERY PLAN. The index it depends on is chosen conditionally by
// the planner, which makes "does this use the index" a real question with a
// checkable answer -- and one that would otherwise regress into a table scan
// without anything failing.
//
// One statement with an artifact_type predicate that is a tautology when no filter
// was asked for, rather than two statements assembled by string concatenation. The
// empty string is not a legal artifact type, so it is unambiguous as "no filter".
//
// Ordering by created_at is not required by the spec, which says nothing about
// order. It is here so the response is stable across calls -- an unordered SQL
// result is entitled to differ between identical queries -- and digest breaks ties,
// since two referrers pushed in the same clock tick would otherwise still be
// unordered.
//
// Both keys descend, which looks like an odd way to break a tie and is deliberate.
// One traversal of one index can satisfy only an ORDER BY whose directions agree, so
// "created_at DESC, digest ASC" would be a seek followed by a sort of the result.
// Nothing depends on which referrer wins a tie, only that the same one always does.
//
// The subject != ” term is doing two jobs and neither is redundant.
//
// It excludes manifests that are not referrers at all: those are stored with an
// empty subject, so a caller passing an empty digest would otherwise be handed
// every plain manifest in the repository as though each referred to nothing.
//
// It is also what makes manifests_by_subject usable. SQLite chooses a partial index
// only when the query's WHERE clause implies the index's own predicate, and
// "subject = ?" against a bound parameter does not -- the planner cannot know the
// parameter is non-empty. Stating it turns a scan into a seek.
const referrersQuery = `
SELECT repository, digest, media_type, artifact_type, subject, annotations, size, created_at
FROM manifests
WHERE repository = ?
  AND subject = ?
  AND subject != ''
  AND (? = '' OR artifact_type = ?)
ORDER BY created_at DESC, digest DESC`

// Referrers lists the manifests in a repository whose subject is dgst, newest
// first, optionally restricted to one artifact type.
//
// The subject is not required to exist. A signature is often pushed before the
// thing it signs finishes uploading, and a referrer whose subject was later
// deleted is still a fact the registry holds; refusing to list either would make
// the endpoint's answer depend on something it is not being asked about.
func (s *Store) Referrers(
	ctx context.Context,
	repository string,
	subject digest.Digest,
	artifactType string,
) ([]model.Manifest, error) {
	rows, err := s.db.QueryContext(ctx, referrersQuery,
		repository, subject.String(), artifactType, artifactType,
	)
	if err != nil {
		return nil, fmt.Errorf("query referrers: %w", err)
	}
	defer rows.Close()

	var referrers []model.Manifest

	for rows.Next() {
		m, err := scanManifest(rows)
		if err != nil {
			return nil, err
		}

		referrers = append(referrers, m)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate referrers: %w", err)
	}

	return referrers, nil
}

// scanManifest reads one manifest row. Taking the narrowest interface both
// *sql.Row and *sql.Rows satisfy keeps the single-row and multi-row paths on one
// column order, which is the thing that silently breaks when they drift.
func scanManifest(row interface{ Scan(...any) error }) (model.Manifest, error) {
	var (
		m           model.Manifest
		dgstText    string
		subject     string
		annotations string
		createdAt   string
	)

	err := row.Scan(
		&m.Repository, &dgstText, &m.MediaType, &m.ArtifactType,
		&subject, &annotations, &m.Size, &createdAt,
	)
	if err != nil {
		// Passed through unwrapped so callers can still test for sql.ErrNoRows.
		if errors.Is(err, sql.ErrNoRows) {
			return model.Manifest{}, err
		}

		return model.Manifest{}, fmt.Errorf("scan manifest: %w", err)
	}

	m.Digest = digest.Digest(dgstText)
	m.Subject = digest.Digest(subject)

	if m.Annotations, err = decodeAnnotations(annotations); err != nil {
		return model.Manifest{}, err
	}

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

// Annotations are stored as a JSON object, which is how they arrived and how they
// leave. Normalising them into a key-value table would let them be queried, but
// nothing queries them: the only reader is a referrers response that reproduces
// the map whole.
//
// The empty string, not "{}", stands for none. It keeps a manifest with no
// annotations from paying two bytes per row, and makes the column's zero value and
// Go's nil map the same thing.
func encodeAnnotations(a map[string]string) (string, error) {
	if len(a) == 0 {
		return "", nil
	}

	encoded, err := json.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("encode annotations: %w", err)
	}

	return string(encoded), nil
}

func decodeAnnotations(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}

	var a map[string]string
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		return nil, fmt.Errorf("decode annotations: %w", err)
	}

	return a, nil
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
