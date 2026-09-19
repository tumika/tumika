package sqlite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sqlc's SQLite parser computes statement offsets inconsistently between bytes
// and runes, so a single multi-byte character in a COMMENT silently shifts every
// statement after it in the generated file. The damage is quiet and severe: an
// em dash in one comment produced
//
//	const setProviderEnabled = `-- name: SetProviderEnabled :exec
//	t;
//	UPDATE providers ... WHERE id =
//
// which compiles, passes review, and fails at runtime with "no such column".
//
// This is cheaper than remembering, and it fails on the file rather than on a
// mangled query hours later.
func TestQueryFilesAreASCII(t *testing.T) {
	roots := []string{
		filepath.Join("..", "queries"),
		filepath.Join("..", "migrations"),
	}

	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read %s: %v", root, err)
		}

		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".sql") {
				continue
			}

			path := filepath.Join(root, entry.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}

			for i, line := range strings.Split(string(raw), "\n") {
				for _, r := range line {
					if r > 127 {
						t.Errorf("%s:%d contains a non-ASCII character (%q); "+
							"sqlc mis-computes statement offsets and will silently truncate the generated SQL",
							path, i+1, r)
						break
					}
				}
			}
		}
	}
}

// The generated queries must be complete statements. A truncated one is
// syntactically plausible Go and only fails when the query runs.
func TestGeneratedQueriesAreNotTruncated(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read .: %v", err)
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql.go") {
			continue
		}

		raw, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}

		for _, block := range strings.Split(string(raw), "= `")[1:] {
			query, _, ok := strings.Cut(block, "`")
			if !ok {
				continue
			}
			trimmed := strings.TrimSpace(query)

			// Every query ends in a complete clause. A shifted offset leaves a
			// dangling operator or an orphaned fragment.
			if strings.HasSuffix(trimmed, "=") || strings.HasSuffix(trimmed, ",") ||
				strings.HasSuffix(trimmed, "(") {
				t.Errorf("%s contains a truncated query ending in %q:\n%s",
					entry.Name(), trimmed[len(trimmed)-1:], trimmed)
			}
			if strings.HasPrefix(trimmed, ";") {
				t.Errorf("%s contains a query starting mid-statement:\n%s", entry.Name(), trimmed)
			}
		}
	}
}
