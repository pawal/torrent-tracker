-- A name that has never resolved was re-queried every hour forever. These count
-- the consecutive passes that produced no address and date the streak, which is
-- what backing off and retiring both need to measure. Existing rows start the
-- streak at the next pass, so upgrading retires nothing.
ALTER TABLE trackers ADD COLUMN resolve_fails INTEGER NOT NULL DEFAULT 0;
ALTER TABLE trackers ADD COLUMN failing_since TEXT;
