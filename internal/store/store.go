// Package store holds the SQLite persistence layer: the tracker registry, the
// address-interval history, and the append-only change feed.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Status is the outcome of resolving a tracker name.
type Status string

const (
	StatusOK       Status = "ok"       // at least one address returned
	StatusNoData   Status = "nodata"   // name exists, but has no A/AAAA
	StatusNXDomain Status = "nxdomain" // name does not exist
	StatusServFail Status = "servfail" // resolver failed to answer
	StatusTimeout  Status = "timeout"  // no answer in time
	StatusError    Status = "error"    // anything else
)

// Resolved reports whether the status represents a usable answer from the
// resolver, as opposed to a failure that says nothing about the name.
func (s Status) Resolved() bool {
	return s == StatusOK || s == StatusNoData || s == StatusNXDomain
}

// Change types recorded in the changes table.
const (
	ChangeIPAdded       = "ip_added"
	ChangeIPRemoved     = "ip_removed"
	ChangeStatusChanged = "status_changed"
	ChangeTrackerAdded  = "tracker_added"
	// ChangeASNChanged records an address moving between origin ASes without
	// the address itself changing.
	ChangeASNChanged = "asn_changed"
)

// Tracker is a known tracker hostname.
type Tracker struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Source        string     `json:"source"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	LastStatus    Status     `json:"last_status"`
	LastCheckedAt *time.Time `json:"last_checked_at"`
}

// IPRecord is one contiguous period during which an address was observed.
type IPRecord struct {
	ID        int64     `json:"id"`
	TrackerID int64     `json:"tracker_id"`
	IP        string    `json:"ip"`
	Family    int       `json:"family"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Active    bool      `json:"active"`
	MissCount int       `json:"-"`
}

// Change is a single entry in the change feed.
type Change struct {
	ID         int64     `json:"id"`
	TrackerID  int64     `json:"tracker_id"`
	Tracker    string    `json:"tracker"`
	ObservedAt time.Time `json:"observed_at"`
	Type       string    `json:"type"`
	IP         string    `json:"ip,omitempty"`
	Family     int       `json:"family,omitempty"`
	Detail     string    `json:"detail,omitempty"`
}

// Run is the metadata for one collection pass.
type Run struct {
	ID           int64      `json:"id"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	TrackerCount int        `json:"tracker_count"`
	OKCount      int        `json:"ok_count"`
	ErrorCount   int        `json:"error_count"`
	ChangeCount  int        `json:"change_count"`
}

// Stats summarises the database for the dashboard.
type Stats struct {
	Trackers        int            `json:"trackers"`
	EnabledTrackers int            `json:"enabled_trackers"`
	ActiveIPs       int            `json:"active_ips"`
	TotalIPRecords  int            `json:"total_ip_records"`
	Changes         int            `json:"changes"`
	ByStatus        map[Status]int `json:"by_status"`
	LastRun         *Run           `json:"last_run"`
}

// Store is a handle on the SQLite database.
type Store struct {
	db *sql.DB
}

// memCounter names in-memory databases uniquely. Without a distinct name each
// shared-cache ":memory:" handle would attach to the same database.
var memCounter atomic.Uint64

// Open opens (creating if needed) the database at path and applies migrations.
// Use ":memory:" for a private ephemeral database.
func Open(path string) (*Store, error) {
	// _txlock=immediate: lock upgrades fail with SQLITE_BUSY without waiting,
	// so writers must take the write lock upfront for busy_timeout to apply.
	const opts = "_pragma=busy_timeout(10000)&_txlock=immediate"

	var dsn string
	if path == ":memory:" {
		// A shared cache keeps the schema visible across pooled connections;
		// the unique name keeps separate Open calls from colliding.
		dsn = fmt.Sprintf("file:memdb%d?mode=memory&cache=shared&%s", memCounter.Add(1), opts)
	} else {
		dsn = "file:" + path + "?_pragma=journal_mode(WAL)&" + opts
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if path == ":memory:" {
		// All goroutines must share the one in-memory database.
		db.SetMaxOpenConns(1)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle for tests and ad-hoc queries.
func (s *Store) DB() *sql.DB { return s.db }

// migrate applies every embedded migration whose index exceeds user_version.
func (s *Store) migrate() error {
	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(entries)

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i, name := range entries {
		if i < version {
			continue
		}
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		// PRAGMA does not accept bound parameters.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("bump schema version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// timestamps are stored as RFC3339 in UTC so they sort lexicographically.
func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func parseNullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Family returns 4 or 6 for an address string.
func Family(ip string) int {
	if strings.Contains(ip, ":") {
		return 6
	}
	return 4
}

// Stats gathers dashboard counters.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{ByStatus: map[Status]int{}}
	row := s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM trackers),
		       (SELECT COUNT(*) FROM trackers WHERE enabled = 1),
		       (SELECT COUNT(*) FROM ip_records WHERE active = 1),
		       (SELECT COUNT(*) FROM ip_records),
		       (SELECT COUNT(*) FROM changes)`)
	if err := row.Scan(&st.Trackers, &st.EnabledTrackers, &st.ActiveIPs, &st.TotalIPRecords, &st.Changes); err != nil {
		return st, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT last_status, COUNT(*) FROM trackers WHERE enabled = 1 GROUP BY last_status`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var status Status
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return st, err
		}
		if status == "" {
			status = "unchecked"
		}
		st.ByStatus[status] = n
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	runs, err := s.RecentRuns(ctx, 1)
	if err != nil {
		return st, err
	}
	if len(runs) > 0 {
		st.LastRun = &runs[0]
	}
	return st, nil
}
