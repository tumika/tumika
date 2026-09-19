// Package migrations embeds the goose migrations so a released binary carries
// its own schema. There is no runtime goose CLI and no migrations directory to
// ship alongside the binary — a self-updating daemon that had to find its
// migrations on disk would be one bad install away from an unmigratable
// database.
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// FS holds every migration, in goose's numeric-prefix naming.
//
//go:embed *.sql
var FS embed.FS

// MaxVersion is the newest migration this binary knows about.
//
// It is what makes the schema-version guard possible: if the database has been
// migrated past this number, it was written by a newer tumika, and this one
// must refuse to start rather than operate against a schema it does not
// understand. That is the safety net for a rollback after a failed update
// (ADR-0003) — an old binary meeting a new database stops loudly instead of
// corrupting data.
func MaxVersion() (int64, error) {
	entries, err := fs.Glob(FS, "*.sql")
	if err != nil {
		return 0, fmt.Errorf("list migrations: %w", err)
	}
	if len(entries) == 0 {
		return 0, fmt.Errorf("no migrations embedded")
	}

	var max int64
	for _, name := range entries {
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return 0, fmt.Errorf("migration %q has no version prefix", name)
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migration %q has a non-numeric version prefix: %w", name, err)
		}
		if v > max {
			max = v
		}
	}
	return max, nil
}
