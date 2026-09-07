package sqlite

// An in-package test, unlike the rest of the suite, because what it checks is not
// behaviour: the referrers listing returns the same rows whether the planner seeks
// an index or scans the table. Only the cost differs, and cost is not observable
// through the exported API.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReferrersQueryUsesTheSubjectIndex is the one assertion behind the claim that
// makes this package worth having: listing what refers to a manifest is a seek, not
// a walk of every manifest in the repository.
//
// Two things this catches, both of which fail silently in every other test because
// the rows returned are identical either way:
//
// manifests_by_subject is *partial*, and SQLite uses a partial index only when the
// query's WHERE clause implies the index's predicate. Dropping the apparently
// redundant "subject != ”" term -- an obvious thing for a later reader to tidy
// away -- turns the seek back into a scan.
//
// The planner also has to prefer it, and without statistics it does not: a prefix
// seek of the primary key on repository alone looks competitive to it, so an
// unanalysed database reads every manifest in the repository. Which is why the
// fixture below has enough rows for the distinction to exist and calls Optimize the
// way Open and Close do.
func TestReferrersQueryUsesTheSubjectIndex(t *testing.T) {
	ctx := context.Background()

	store, err := Open(ctx, filepath.Join(t.TempDir(), "cairn.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() { store.Close() })

	const repository = "acme/widgets"

	subject := "sha256:" + strings.Repeat("a", 64)

	// Lopsided on purpose. Thousands of ordinary manifests are what the wrong plan
	// would read, and a handful of referrers is what the right one returns; with a few
	// rows of each the two plans cost the same and the planner's choice says nothing.
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	insert := `
INSERT INTO manifests
    (repository, digest, media_type, artifact_type, subject, annotations, size, created_at)
VALUES (?, ?, 'application/vnd.oci.image.manifest.v1+json', ?, ?, '', 1, ?)`

	now := formatTime(time.Now())

	for i := range 5000 {
		if _, err := tx.ExecContext(ctx, insert,
			repository, fmt.Sprintf("sha256:%064d", i), "", "", now); err != nil {
			t.Fatalf("insert manifest: %v", err)
		}
	}

	for i := range 20 {
		if _, err := tx.ExecContext(ctx, insert,
			repository, fmt.Sprintf("sha256:%064x", 1<<40+i), "sig", subject, now); err != nil {
			t.Fatalf("insert referrer: %v", err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := store.Optimize(ctx); err != nil {
		t.Fatalf("optimize: %v", err)
	}

	got := queryPlan(t, store, referrersQuery, repository, subject, "", "")

	if !strings.Contains(got, "manifests_by_subject") {
		t.Errorf("plan does not use manifests_by_subject:\n%s", got)
	}

	// SEARCH is a seek; SCAN reads every row. This is the failure the test exists for.
	if strings.Contains(got, "SCAN manifests") {
		t.Errorf("plan scans the manifests table:\n%s", got)
	}

	// A sort of the result would undo half the point of putting created_at and digest
	// in the index. It appears the moment the ORDER BY directions stop agreeing.
	if strings.Contains(got, "TEMP B-TREE") {
		t.Errorf("plan sorts instead of walking the index in order:\n%s", got)
	}
}

func queryPlan(t *testing.T, store *Store, query string, args ...any) string {
	t.Helper()

	rows, err := store.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}

	defer rows.Close()

	var plan strings.Builder

	for rows.Next() {
		var (
			id, parent, notUsed int
			detail              string
		)

		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}

		plan.WriteString(detail)
		plan.WriteString("\n")
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}

	return plan.String()
}
