package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

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

	db, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "cairn.db")))
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
