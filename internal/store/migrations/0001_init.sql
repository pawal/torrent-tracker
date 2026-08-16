-- Known tracker hostnames.
CREATE TABLE trackers (
    id              INTEGER PRIMARY KEY,
    name            TEXT    NOT NULL UNIQUE,
    source          TEXT    NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT    NOT NULL,
    last_status     TEXT    NOT NULL DEFAULT '',
    last_checked_at TEXT
);

CREATE INDEX trackers_enabled ON trackers (enabled);

-- Address observations as intervals: one row per contiguous period an address
-- was seen for a tracker. A readdressed-then-restored IP gets a fresh row.
CREATE TABLE ip_records (
    id         INTEGER PRIMARY KEY,
    tracker_id INTEGER NOT NULL REFERENCES trackers (id) ON DELETE CASCADE,
    ip         TEXT    NOT NULL,
    family     INTEGER NOT NULL,
    first_seen TEXT    NOT NULL,
    last_seen  TEXT    NOT NULL,
    active     INTEGER NOT NULL DEFAULT 1,
    miss_count INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX ip_records_tracker ON ip_records (tracker_id, active);
-- An address can recur over time, but only one interval may be open at once.
CREATE UNIQUE INDEX ip_records_active_unique
    ON ip_records (tracker_id, ip) WHERE active = 1;

-- Append-only change feed: what the frontend reads.
CREATE TABLE changes (
    id          INTEGER PRIMARY KEY,
    tracker_id  INTEGER NOT NULL REFERENCES trackers (id) ON DELETE CASCADE,
    observed_at TEXT    NOT NULL,
    change_type TEXT    NOT NULL,
    ip          TEXT,
    family      INTEGER,
    detail      TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX changes_observed ON changes (observed_at DESC, id DESC);
CREATE INDEX changes_tracker ON changes (tracker_id, observed_at DESC);

-- Per-lookup audit trail, kept so a resolver outage is distinguishable from
-- trackers genuinely going away.
CREATE TABLE lookups (
    id          INTEGER PRIMARY KEY,
    tracker_id  INTEGER NOT NULL REFERENCES trackers (id) ON DELETE CASCADE,
    ts          TEXT    NOT NULL,
    status      TEXT    NOT NULL,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    error       TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX lookups_tracker ON lookups (tracker_id, ts DESC);

-- Collection run metadata.
CREATE TABLE runs (
    id            INTEGER PRIMARY KEY,
    started_at    TEXT    NOT NULL,
    finished_at   TEXT,
    tracker_count INTEGER NOT NULL DEFAULT 0,
    ok_count      INTEGER NOT NULL DEFAULT 0,
    error_count   INTEGER NOT NULL DEFAULT 0,
    change_count  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX runs_started ON runs (started_at DESC);
