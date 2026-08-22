package store

import (
	"context"
	"fmt"
)

// scanner is whatever a query hands a row to: *sql.Row or *sql.Rows.
type scanner interface{ Scan(...any) error }

// eachRow runs q and hands every row to fn.
func (s *Store) eachRow(ctx context.Context, q string, args []any, fn func(scanner) error) error {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// queryAll scans every row of q into a slice, empty rather than nil so a JSON
// response carries [] instead of null.
func queryAll[T any](ctx context.Context, s *Store, scan func(scanner) (T, error), q string, args ...any) ([]T, error) {
	out := []T{}
	err := s.eachRow(ctx, q, args, func(sc scanner) error {
		v, err := scan(sc)
		if err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// execOne runs a statement that must touch a row, reporting ErrNotFound when it
// touched none.
func (s *Store) execOne(ctx context.Context, name, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	return nil
}

// eachRollingFamily hands fn every family currently tracked by prefix. Both the
// list view and the probing plan need to know which ones they are.
func (s *Store) eachRollingFamily(ctx context.Context, fn func(trackerID int64, family int)) error {
	return s.eachRow(ctx,
		`SELECT tracker_id, family FROM family_state WHERE rolling = 1 ORDER BY family`,
		nil, func(sc scanner) error {
			var (
				id     int64
				family int
			)
			if err := sc.Scan(&id, &family); err != nil {
				return err
			}
			fn(id, family)
			return nil
		})
}

// scanString reads a single-column row, for the queries that select one value.
func scanString(sc scanner) (string, error) {
	var v string
	err := sc.Scan(&v)
	return v, err
}
