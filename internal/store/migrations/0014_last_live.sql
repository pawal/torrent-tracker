-- Half the registry reads dead while DNS reads ok, and almost all of it is
-- parked domains that never were trackers. "Never once answered" and "answered
-- before, then stopped" are different facts about a name, so this dates the
-- last answer: NULL means there has never been one. The probe history already
-- holds it, so existing rows are derived rather than started blank.
ALTER TABLE trackers ADD COLUMN last_live_at TEXT;

UPDATE trackers SET last_live_at = (
    SELECT MAX(at) FROM (
        SELECT p.checked_at AS at FROM probes p
          JOIN endpoints e ON e.id = p.endpoint_id
         WHERE e.tracker_id = trackers.id AND p.result = 'live'
        UNION ALL
        SELECT h.until AS at FROM probe_history h
          JOIN endpoints e ON e.id = h.endpoint_id
         WHERE e.tracker_id = trackers.id AND h.result = 'live'
    )
);
