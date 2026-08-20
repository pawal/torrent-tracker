-- Reachability history per address. The probes table holds only the open
-- interval, so a verdict that changes would otherwise be lost; a closed
-- interval is appended here on every transition.
CREATE TABLE probe_history (
    id          INTEGER PRIMARY KEY,
    endpoint_id INTEGER NOT NULL REFERENCES endpoints (id) ON DELETE CASCADE,
    ip          TEXT    NOT NULL,
    family      INTEGER NOT NULL,
    result      TEXT    NOT NULL,
    reason      TEXT    NOT NULL DEFAULT '',
    -- When the verdict started and when it was replaced, the same interval
    -- shape ip_records uses for addresses.
    since       TEXT    NOT NULL,
    until       TEXT    NOT NULL
);

CREATE INDEX probe_history_endpoint ON probe_history (endpoint_id, ip, since);
CREATE INDEX probe_history_until ON probe_history (until);
