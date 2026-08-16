package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IPInfo is the stored network placement of a single address.
type IPInfo struct {
	IP          string    `json:"ip"`
	Family      int       `json:"family"`
	ASN         int       `json:"asn,omitempty"`
	ASName      string    `json:"as_name,omitempty"`
	Prefix      string    `json:"prefix,omitempty"`
	RIR         string    `json:"rir,omitempty"`
	Country     string    `json:"country,omitempty"`
	Allocated   string    `json:"allocated,omitempty"`
	NetworkName string    `json:"network_name,omitempty"`
	Org         string    `json:"org,omitempty"`
	City        string    `json:"city,omitempty"`
	Latitude    float64   `json:"latitude,omitempty"`
	Longitude   float64   `json:"longitude,omitempty"`
	Sources     string    `json:"sources,omitempty"`
	FetchedAt   time.Time `json:"fetched_at"`
	Error       string    `json:"error,omitempty"`
}

// Holder is the most useful human-readable owner of the address.
func (i IPInfo) Holder() string {
	for _, s := range []string{i.Org, i.ASName, i.NetworkName} {
		if s != "" {
			return s
		}
	}
	return ""
}

const ipInfoColumns = `ip, family, asn, as_name, prefix, rir, country, allocated,
	network_name, org, city, latitude, longitude, sources, fetched_at, error`

func scanIPInfo(sc interface{ Scan(...any) error }) (IPInfo, error) {
	var (
		i       IPInfo
		fetched string
	)
	err := sc.Scan(&i.IP, &i.Family, &i.ASN, &i.ASName, &i.Prefix, &i.RIR, &i.Country,
		&i.Allocated, &i.NetworkName, &i.Org, &i.City, &i.Latitude, &i.Longitude,
		&i.Sources, &fetched, &i.Error)
	if err != nil {
		return i, err
	}
	i.FetchedAt, err = parseTime(fetched)
	return i, err
}

// IPInfoFor returns the stored placement for one address.
func (s *Store) IPInfoFor(ctx context.Context, ip string) (IPInfo, error) {
	info, err := scanIPInfo(s.db.QueryRowContext(ctx,
		`SELECT `+ipInfoColumns+` FROM ip_info WHERE ip = ?`, ip))
	if errors.Is(err, sql.ErrNoRows) {
		return IPInfo{}, fmt.Errorf("%q: %w", ip, ErrNotFound)
	}
	return info, err
}

// PutIPInfo upserts an address's placement. A changed origin AS is recorded in
// the change feed against every tracker currently pointing at the address.
func (s *Store) PutIPInfo(ctx context.Context, info IPInfo, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var (
		prevASN  int
		prevName string
		existed  bool
	)
	err = tx.QueryRowContext(ctx, `SELECT asn, as_name FROM ip_info WHERE ip = ?`, info.IP).
		Scan(&prevASN, &prevName)
	switch {
	case err == nil:
		existed = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return err
	}

	ts := fmtTime(now)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO ip_info (`+ipInfoColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ip) DO UPDATE SET
			family = excluded.family, asn = excluded.asn, as_name = excluded.as_name,
			prefix = excluded.prefix, rir = excluded.rir, country = excluded.country,
			allocated = excluded.allocated, network_name = excluded.network_name,
			org = excluded.org, city = excluded.city, latitude = excluded.latitude,
			longitude = excluded.longitude, sources = excluded.sources,
			fetched_at = excluded.fetched_at, error = excluded.error`,
		info.IP, info.Family, info.ASN, info.ASName, info.Prefix, info.RIR, info.Country,
		info.Allocated, info.NetworkName, info.Org, info.City, info.Latitude, info.Longitude,
		info.Sources, ts, info.Error)
	if err != nil {
		return fmt.Errorf("upsert ip_info %s: %w", info.IP, err)
	}

	// Only a genuine move counts: ignore the first observation, and ignore a
	// lookup that simply failed to determine the AS this time round.
	if existed && prevASN != 0 && info.ASN != 0 && prevASN != info.ASN {
		detail := fmt.Sprintf("%s: AS%d %s -> AS%d %s",
			info.IP, prevASN, orUnknown(prevName), info.ASN, orUnknown(info.ASName))

		rows, err := tx.QueryContext(ctx, `
			SELECT tracker_id FROM ip_records WHERE ip = ? AND active = 1`, info.IP)
		if err != nil {
			return err
		}
		var trackerIDs []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			trackerIDs = append(trackerIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, id := range trackerIDs {
			if err := insertChange(ctx, tx, id, ts, ChangeASNChanged, info.IP, info.Family, detail); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// IPsNeedingEnrichment returns active addresses that have never been enriched
// or whose data is older than maxAge, oldest first.
func (s *Store) IPsNeedingEnrichment(ctx context.Context, maxAge time.Duration, now time.Time, limit int) ([]IPRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	cutoff := fmtTime(now.Add(-maxAge))

	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT r.ip, r.family
		FROM ip_records r
		LEFT JOIN ip_info i ON i.ip = r.ip
		WHERE r.active = 1 AND r.is_prefix = 0 AND (i.ip IS NULL OR i.fetched_at < ?)
		ORDER BY COALESCE(i.fetched_at, ''), r.ip
		LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []IPRecord{}
	for rows.Next() {
		var r IPRecord
		if err := rows.Scan(&r.IP, &r.Family); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// IPInfoForTracker returns placement keyed by address for one tracker's whole
// history. Prefix records are keyed by CIDR, taking any one address inside.
func (s *Store) IPInfoForTracker(ctx context.Context, trackerID int64) (map[string]IPInfo, error) {
	out := map[string]IPInfo{}

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+prefixed(ipInfoColumns, "i")+`
		FROM ip_info i
		WHERE i.ip IN (SELECT ip FROM ip_records WHERE tracker_id = ?)`, trackerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		info, err := scanIPInfo(rows)
		if err != nil {
			return nil, err
		}
		out[info.IP] = info
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	prefixes, err := s.db.QueryContext(ctx, `
		SELECT `+prefixed(ipInfoColumns, "i")+`
		FROM ip_info i
		WHERE i.prefix IN (SELECT ip FROM ip_records WHERE tracker_id = ? AND is_prefix = 1)
		GROUP BY i.prefix`, trackerID)
	if err != nil {
		return nil, err
	}
	defer prefixes.Close()

	for prefixes.Next() {
		info, err := scanIPInfo(prefixes)
		if err != nil {
			return nil, err
		}
		info.IP = info.Prefix
		out[info.Prefix] = info
	}
	return out, prefixes.Err()
}

// AllIPInfo returns placement for every currently active address, keyed by
// address, for the list view.
func (s *Store) AllIPInfo(ctx context.Context) (map[string]IPInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+prefixed(ipInfoColumns, "i")+`
		FROM ip_info i
		WHERE i.ip IN (SELECT ip FROM ip_records WHERE active = 1)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]IPInfo{}
	for rows.Next() {
		info, err := scanIPInfo(rows)
		if err != nil {
			return nil, err
		}
		out[info.IP] = info
	}
	return out, rows.Err()
}

// PrefixMap returns the prefix each enriched address sits in.
func (s *Store) PrefixMap(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ip, prefix FROM ip_info WHERE prefix != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var ip, prefix string
		if err := rows.Scan(&ip, &prefix); err != nil {
			return nil, err
		}
		out[ip] = prefix
	}
	return out, rows.Err()
}

// prefixed qualifies a bare column list with a table alias.
func prefixed(columns, alias string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// recordInfoJoin ties a record to its enrichment: addresses join on the
// address, prefix records on the prefix they stand for.
const recordInfoJoin = `JOIN ip_info i ON (r.is_prefix = 0 AND i.ip = r.ip)
		                     OR (r.is_prefix = 1 AND i.prefix = r.ip)`

// NetworkStat aggregates active addresses by network or registry.
type NetworkStat struct {
	Key      string `json:"key"`
	Label    string `json:"label,omitempty"`
	Trackers int    `json:"trackers"`
	IPs      int    `json:"ips"`
}

// TopNetworks summarises which ASes the tracked hosts actually live in.
func (s *Store) TopNetworks(ctx context.Context, limit int) ([]NetworkStat, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	return s.networkStats(ctx, `
		SELECT CASE WHEN i.asn = 0 THEN 'unknown' ELSE 'AS' || i.asn END,
		       COALESCE(NULLIF(i.org, ''), NULLIF(i.as_name, ''), ''),
		       COUNT(DISTINCT r.tracker_id), COUNT(DISTINCT r.ip)
		FROM ip_records r
		`+recordInfoJoin+`
		WHERE r.active = 1
		GROUP BY i.asn
		ORDER BY COUNT(DISTINCT r.tracker_id) DESC, COUNT(DISTINCT r.ip) DESC
		LIMIT ?`, limit)
}

// ByRIR summarises active addresses per allocating registry.
func (s *Store) ByRIR(ctx context.Context) ([]NetworkStat, error) {
	return s.networkStats(ctx, `
		SELECT CASE WHEN i.rir = '' THEN 'unknown' ELSE i.rir END, '',
		       COUNT(DISTINCT r.tracker_id), COUNT(DISTINCT r.ip)
		FROM ip_records r
		`+recordInfoJoin+`
		WHERE r.active = 1
		GROUP BY i.rir
		ORDER BY COUNT(DISTINCT r.tracker_id) DESC`)
}

// ByCountry summarises active addresses per country.
func (s *Store) ByCountry(ctx context.Context, limit int) ([]NetworkStat, error) {
	if limit <= 0 || limit > 500 {
		limit = 30
	}
	return s.networkStats(ctx, `
		SELECT CASE WHEN i.country = '' THEN 'unknown' ELSE i.country END, '',
		       COUNT(DISTINCT r.tracker_id), COUNT(DISTINCT r.ip)
		FROM ip_records r
		`+recordInfoJoin+`
		WHERE r.active = 1
		GROUP BY i.country
		ORDER BY COUNT(DISTINCT r.tracker_id) DESC
		LIMIT ?`, limit)
}

func (s *Store) networkStats(ctx context.Context, q string, args ...any) ([]NetworkStat, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []NetworkStat{}
	for rows.Next() {
		var n NetworkStat
		if err := rows.Scan(&n.Key, &n.Label, &n.Trackers, &n.IPs); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// EnrichmentCoverage reports how many active addresses have been enriched.
type EnrichmentCoverage struct {
	ActiveIPs int `json:"active_ips"`
	Enriched  int `json:"enriched"`
	WithASN   int `json:"with_asn"`
}

// Coverage reports enrichment progress for the dashboard.
func (s *Store) Coverage(ctx context.Context) (EnrichmentCoverage, error) {
	var c EnrichmentCoverage
	err := s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(DISTINCT ip) FROM ip_records WHERE active = 1 AND is_prefix = 0),
		       (SELECT COUNT(DISTINCT r.ip) FROM ip_records r JOIN ip_info i ON i.ip = r.ip WHERE r.active = 1),
		       (SELECT COUNT(DISTINCT r.ip) FROM ip_records r JOIN ip_info i ON i.ip = r.ip WHERE r.active = 1 AND i.asn != 0)`).
		Scan(&c.ActiveIPs, &c.Enriched, &c.WithASN)
	return c, err
}

// NetworkRef is a distinct network a tracker's addresses sit in.
type NetworkRef struct {
	ASN     int    `json:"asn,omitempty"`
	Holder  string `json:"holder,omitempty"`
	RIR     string `json:"rir,omitempty"`
	Country string `json:"country,omitempty"`
}

// TrackerNetworks returns the distinct networks behind each tracker's active
// addresses, keyed by tracker id.
func (s *Store) TrackerNetworks(ctx context.Context) (map[int64][]NetworkRef, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT r.tracker_id, i.asn,
		       COALESCE(NULLIF(i.org, ''), NULLIF(i.as_name, ''), i.network_name),
		       i.rir, i.country
		FROM ip_records r
		`+recordInfoJoin+`
		WHERE r.active = 1
		ORDER BY r.tracker_id, i.asn`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64][]NetworkRef{}
	for rows.Next() {
		var (
			id  int64
			ref NetworkRef
		)
		if err := rows.Scan(&id, &ref.ASN, &ref.Holder, &ref.RIR, &ref.Country); err != nil {
			return nil, err
		}
		out[id] = append(out[id], ref)
	}
	return out, rows.Err()
}
