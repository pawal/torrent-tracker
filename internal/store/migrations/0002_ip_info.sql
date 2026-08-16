-- Network placement for observed addresses: origin AS, allocating RIR and
-- geolocation. Keyed by address rather than by interval, because the facts
-- describe the address itself; re-enrichment overwrites in place and any AS
-- change is recorded in the change feed.
CREATE TABLE ip_info (
    ip           TEXT    PRIMARY KEY,
    family       INTEGER NOT NULL,
    asn          INTEGER NOT NULL DEFAULT 0,
    as_name      TEXT    NOT NULL DEFAULT '',
    prefix       TEXT    NOT NULL DEFAULT '',
    rir          TEXT    NOT NULL DEFAULT '',
    country      TEXT    NOT NULL DEFAULT '',
    allocated    TEXT    NOT NULL DEFAULT '',
    network_name TEXT    NOT NULL DEFAULT '',
    org          TEXT    NOT NULL DEFAULT '',
    city         TEXT    NOT NULL DEFAULT '',
    latitude     REAL    NOT NULL DEFAULT 0,
    longitude    REAL    NOT NULL DEFAULT 0,
    sources      TEXT    NOT NULL DEFAULT '',
    fetched_at   TEXT    NOT NULL,
    error        TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX ip_info_asn ON ip_info (asn);
CREATE INDEX ip_info_rir ON ip_info (rir);
CREATE INDEX ip_info_country ON ip_info (country);
-- Drives "which addresses need re-checking" without a table scan.
CREATE INDEX ip_info_fetched ON ip_info (fetched_at);
