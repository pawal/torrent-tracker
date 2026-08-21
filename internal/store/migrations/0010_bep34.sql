-- BEP 34 tracker preferences. A BITTORRENT TXT record on the tracker's own
-- hostname names the ports it runs trackers on; one naming none says it runs
-- no trackers at all, which is the spec's opt-out and the only way an operator
-- has of telling a monitor to stop.
ALTER TABLE trackers ADD COLUMN bep34        TEXT    NOT NULL DEFAULT '';
ALTER TABLE trackers ADD COLUMN bep34_denies INTEGER NOT NULL DEFAULT 0;
