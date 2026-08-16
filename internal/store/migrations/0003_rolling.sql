-- Rolling families: hosts behind a CDN answer with a fresh set of addresses
-- every time their TTL expires. Recorded one row at a time that is thousands
-- of rows a year saying the same thing, so a family whose address set keeps
-- changing is stored as the prefix its addresses sit in instead.
ALTER TABLE ip_records ADD COLUMN is_prefix INTEGER NOT NULL DEFAULT 0;

-- One row per (tracker, family) carrying the churn bookkeeping. fingerprint is
-- of the last observed address set, so churn stays measurable while the family
-- is stored as prefixes.
CREATE TABLE family_state (
    tracker_id  INTEGER NOT NULL REFERENCES trackers (id) ON DELETE CASCADE,
    family      INTEGER NOT NULL,
    fingerprint TEXT    NOT NULL DEFAULT '',
    churn       INTEGER NOT NULL DEFAULT 0,
    steady      INTEGER NOT NULL DEFAULT 0,
    rolling     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tracker_id, family)
);

-- Prefix records join enrichment on the prefix rather than the address.
CREATE INDEX ip_info_prefix ON ip_info (prefix);
