-- Announce endpoints. A hostname alone cannot be probed: one tracker commonly
-- serves udp:6969 and https:443, and they disagree often enough that a single
-- verdict per name would hide it. Harvested from the imported announce URLs,
-- which until now were reduced to a hostname and discarded.
CREATE TABLE endpoints (
    id         INTEGER PRIMARY KEY,
    tracker_id INTEGER NOT NULL REFERENCES trackers (id) ON DELETE CASCADE,
    scheme     TEXT    NOT NULL,
    port       INTEGER NOT NULL,
    path       TEXT    NOT NULL DEFAULT '/announce',
    first_seen TEXT    NOT NULL,
    UNIQUE (tracker_id, scheme, port, path)
);

CREATE INDEX endpoints_tracker ON endpoints (tracker_id);

-- Probe results, one row per (endpoint, address): a name with four A records
-- can be serving on three of them. Current state only, since the up/down
-- history is the change feed's job.
CREATE TABLE probes (
    endpoint_id INTEGER NOT NULL REFERENCES endpoints (id) ON DELETE CASCADE,
    ip          TEXT    NOT NULL,
    family      INTEGER NOT NULL,
    result      TEXT    NOT NULL,
    reason      TEXT    NOT NULL DEFAULT '',
    rtt_ms      INTEGER NOT NULL DEFAULT 0,
    -- Consecutive failures so far. A tracker that drops one UDP packet is not
    -- dead, the same way one absent A record does not retire an address.
    miss_count  INTEGER NOT NULL DEFAULT 0,
    -- When the current result started, so the UI can age a verdict.
    since       TEXT    NOT NULL,
    checked_at  TEXT    NOT NULL,
    PRIMARY KEY (endpoint_id, ip)
);

-- Reachability rolls the probe results up to the name: live when everything
-- probed answers, dead when nothing does, partial in between. A name we could
-- not probe at all stays unknown rather than being called dead.
ALTER TABLE trackers ADD COLUMN reach TEXT NOT NULL DEFAULT '';
ALTER TABLE trackers ADD COLUMN reach_checked_at TEXT;
