package sqlite

import (
	"database/sql"
	"fmt"
	"time"
)

// timeLayout is how every timestamp is written.
//
// Fixed width and always UTC, so the text sorts in chronological order — which
// is what lets ORDER BY and range comparisons work on a TEXT column without a
// conversion. Reading uses time.RFC3339, which accepts both this and the
// second-precision form the initial migration seeds with strftime.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// nullTime renders an optional timestamp. A nil pointer means "not known yet"
// and is stored as NULL — never as a zero time, which would read back as year 1
// and quietly satisfy a "before now" comparison.
func nullTime(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(*t), Valid: true}
}

func parseNullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// SQLite has no boolean type; the schema stores 0/1 with a CHECK.
func boolToInt(b bool) int64 { return map[bool]int64{false: 0, true: 1}[b] }

func intToBool(i int64) bool { return i != 0 }

func nullInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func parseNullInt64(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func nullIntFromPtr(v *int) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}

func parseNullIntPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}
