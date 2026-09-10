package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/tkircsi/cairn/internal/metastore"
	"github.com/tkircsi/cairn/internal/model"
)

// TestPragmasHoldOnEveryConnection is the test that should have existed first.
//
// database/sql hands out connections from a pool, and most PRAGMAs configure a
// connection rather than the database: journal_mode is recorded in the file and
// survives, but busy_timeout and foreign_keys are per-connection and reset to their
// defaults on every new one. Setting them with db.Exec therefore configures whichever
// connection happened to run the statement and no other.
//
// Nothing sequential can notice. One caller at a time reuses one pooled connection --
// the configured one -- so the whole test suite, the conformance suite and a 3000-push
// benchmark all pass while the second connection has never been configured. It takes
// two callers at once to open a second connection, and by then the symptom is a 500
// rather than anything pointing here.
//
// The pool here is the test's own, not the store's, because the store's is capped at one
// connection. The invariant is a property of the DSN -- whatever connection SQLite hands
// back is already configured, however many there are -- so it has to be checked against a
// pool that will open more than one. Asserting it through the store would quietly reduce
// to a test of a single connection, which is the shape of the original bug.
func TestPragmasHoldOnEveryConnection(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "cairn.db"), pragmas))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	// More than one connection, and held open together: asking the pool for two
	// connections in sequence would hand back the same one twice and prove nothing.
	const connections = 4

	type conn struct {
		busyTimeout int
		foreignKeys int
		synchronous int
	}

	var (
		mu      sync.Mutex
		seen    []conn
		release = make(chan struct{})
		ready   sync.WaitGroup
		done    sync.WaitGroup
	)

	ready.Add(connections)
	done.Add(connections)

	for range connections {
		go func() {
			defer done.Done()

			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Error(err)
				ready.Done()

				return
			}

			defer tx.Rollback()

			var c conn

			if err := tx.QueryRow("PRAGMA busy_timeout").Scan(&c.busyTimeout); err != nil {
				t.Error(err)
			}

			if err := tx.QueryRow("PRAGMA foreign_keys").Scan(&c.foreignKeys); err != nil {
				t.Error(err)
			}

			if err := tx.QueryRow("PRAGMA synchronous").Scan(&c.synchronous); err != nil {
				t.Error(err)
			}

			mu.Lock()
			seen = append(seen, c)
			mu.Unlock()

			// Signal only after the connection is pinned by an open transaction, so the
			// others cannot be served by this same one.
			ready.Done()
			<-release
		}()
	}

	ready.Wait()
	close(release)
	done.Wait()

	for i, c := range seen {
		if c.busyTimeout == 0 {
			t.Errorf("connection %d has busy_timeout 0: a concurrent writer fails "+
				"immediately with SQLITE_BUSY instead of waiting", i)
		}

		if c.foreignKeys != 1 {
			t.Errorf("connection %d has foreign_keys off: the schema's cascades are "+
				"silently not enforced on it", i)
		}

		// 1 is NORMAL, 2 is FULL. Asserted by value rather than "not FULL" because
		// this is also the check that the DSN spelling reaches SQLite at all -- a
		// pragma the driver silently ignored would leave the default of 2 here.
		if c.synchronous != 1 {
			t.Errorf("connection %d has synchronous = %d, want 1 (NORMAL): the WAL is "+
				"being fsynced on every commit", i, c.synchronous)
		}
	}
}

// TestStoreSerialisesWrites pins the pool at one connection.
//
// This looks like a test of a setter, and it is there because the value is load-bearing
// and invisible. Removing the cap does not fail anything: every test still passes, the
// conformance suite still passes, and the only symptom is that concurrent writers go back
// to racing SQLite's busy handler and failing under sustained load -- which no test in
// this repository reproduces, because it takes tens of writers and tens of thousands of
// writes to show up.
//
// So the guarantee is asserted directly rather than through its consequences.
func TestStoreSerialisesWrites(t *testing.T) {
	t.Parallel()

	store := open(t)

	if got := store.db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1: concurrent writers will contend for "+
			"SQLite's single write lock through its busy handler rather than queue", got)
	}
}

// TestConcurrentPutBlobDoesNotFail is the failure a client actually sees.
//
// oras uploads a manifest's blobs concurrently, so an ordinary `oras push` closes two
// upload sessions at once, which is two PutBlob calls on two pooled connections. On a
// connection with no busy timeout the second returns SQLITE_BUSY at once, the handler
// turns that into a 500, and oras reports "unsupported: internal error" on a push that
// was entirely valid.
func TestConcurrentPutBlobDoesNotFail(t *testing.T) {
	t.Parallel()

	store := open(t)

	const writers = 8

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed []error
	)

	wg.Add(writers)

	for i := range writers {
		go func() {
			defer wg.Done()

			blob := model.Blob{
				Repository: "acme/widgets",
				Digest:     digest.FromString(string(rune('a' + i))),
				Size:       int64(i),
				CreatedAt:  time.Unix(1700000000, 0).UTC(),
			}

			if err := store.PutBlob(context.Background(), blob); err != nil {
				mu.Lock()
				failed = append(failed, err)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	for _, err := range failed {
		t.Errorf("concurrent PutBlob failed: %v", err)
	}
}

// TestConcurrentPutBlobIsIdempotent pins the other half: concurrent writes of the same
// blob must not turn a duplicate into an error. ON CONFLICT DO NOTHING covers it, and
// this is here so a future rewrite into a select-then-insert has to notice it.
func TestConcurrentPutBlobIsIdempotent(t *testing.T) {
	t.Parallel()

	store := open(t)

	const writers = 8

	blob := model.Blob{
		Repository: "acme/widgets",
		Digest:     digest.FromString("same bytes every time"),
		Size:       11,
		CreatedAt:  time.Unix(1700000000, 0).UTC(),
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed []error
	)

	wg.Add(writers)

	for range writers {
		go func() {
			defer wg.Done()

			if err := store.PutBlob(context.Background(), blob); err != nil {
				mu.Lock()
				failed = append(failed, err)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	for _, err := range failed {
		t.Errorf("concurrent identical PutBlob failed: %v", err)
	}

	got, err := store.Blob(context.Background(), blob.Repository, blob.Digest)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if got.Size != blob.Size {
		t.Errorf("size = %d, want %d", got.Size, blob.Size)
	}
}

// TestContendedWriteIsReportedAsBusy pins the classification the HTTP layer depends on.
//
// SQLITE_BUSY means the request was fine and someone else had the lock, so a client
// that retries succeeds. Reported as a generic write failure it becomes a 500, which
// tells that client the opposite, and the registry claims an internal fault for
// something it could have queued.
//
// The lock is taken from a second database handle rather than a second goroutine,
// because with the pool capped at one connection the store cannot contend with itself.
// What it can still contend with is another process on the same data root -- a second
// cairnd, or a backup reading the file -- which is what this reproduces.
func TestContendedWriteIsReportedAsBusy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cairn.db")

	// Long enough that the wait is real, short enough that the test does not spend the
	// production five seconds proving it.
	store, err := openWith(ctx, path, withBusyTimeout(pragmas, "busy_timeout(100)"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() { store.Close() })

	blocker, err := sql.Open("sqlite", dsn(path, pragmas))
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}

	t.Cleanup(func() { blocker.Close() })

	tx, err := blocker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	defer tx.Rollback() //nolint:errcheck // the lock is released either way

	// A bare BEGIN is deferred in SQLite and takes no write lock until something
	// writes, so the transaction has to actually insert to hold the lock this test
	// needs held.
	if _, err := tx.ExecContext(ctx, `
INSERT INTO blobs (repository, digest, size, created_at) VALUES ('acme/widgets', 'sha256:x', 1, '')`,
	); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}

	err = store.PutBlob(ctx, model.Blob{
		Repository: "acme/widgets",
		Digest:     digest.FromString("contended"),
		Size:       9,
		CreatedAt:  time.Unix(1700000000, 0).UTC(),
	})

	if err == nil {
		t.Fatal("PutBlob succeeded against a held write lock")
	}

	if !errors.Is(err, metastore.ErrBusy) {
		t.Errorf("error is not ErrBusy, so the handler answers 500 and the client "+
			"gives up on a request it should retry: %v", err)
	}

	// The sentinel is wrapped, not substituted. An operator needs the result code to
	// tell real contention from anything else that ends up wearing this status.
	if !strings.Contains(err.Error(), "SQLITE_BUSY") {
		t.Errorf("error lost the driver's message, so the log cannot show why: %v", err)
	}
}

// TestPermanentWriteFailureIsNotBusy is the other half, and the more important one.
//
// A classification that says yes too often is worse than none: answering 503 to a
// permanent failure has the client retry a request that can never succeed. A duplicate
// upload id violates the primary key, which no amount of retrying fixes.
func TestPermanentWriteFailureIsNotBusy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := open(t)

	upload := model.Upload{
		ID:         "1c8b4c4e-0e2a-4d0f-9b3a-2f6d8c1e5a70",
		Repository: "acme/widgets",
		StartedAt:  time.Unix(1700000000, 0).UTC(),
		UpdatedAt:  time.Unix(1700000000, 0).UTC(),
	}

	if err := store.CreateUpload(ctx, upload); err != nil {
		t.Fatalf("first create: %v", err)
	}

	err := store.CreateUpload(ctx, upload)
	if err == nil {
		t.Fatal("duplicate upload id was accepted")
	}

	if errors.Is(err, metastore.ErrBusy) {
		t.Errorf("constraint violation classified as contention, so the client is told "+
			"to retry something that cannot succeed: %v", err)
	}
}

// withBusyTimeout replaces the busy_timeout entry, leaving the rest of the pragmas
// exactly as production runs them. Building a short list by hand would let this test
// drift away from the configuration it is meant to be testing.
func withBusyTimeout(base []string, replacement string) []string {
	out := make([]string, 0, len(base))

	for _, pragma := range base {
		if strings.HasPrefix(pragma, "busy_timeout(") {
			pragma = replacement
		}

		out = append(out, pragma)
	}

	return out
}

// open is a fresh store on disk. Not :memory:, because an in-memory database is
// per-connection and would hide exactly the class of bug these tests are about.
func open(t *testing.T) *Store {
	t.Helper()

	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "cairn.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() {
		if err := store.Close(); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("close: %v", err)
		}
	})

	return store
}
