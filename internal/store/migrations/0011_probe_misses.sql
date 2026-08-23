-- Failed attempts inside one verdict interval. Uptime is measured from the
-- intervals, and a verdict carried by the grace path never breaks, so without
-- this a name failing every other round reads exactly like a clean one.
ALTER TABLE probes ADD COLUMN misses INTEGER NOT NULL DEFAULT 0;
ALTER TABLE probe_history ADD COLUMN misses INTEGER NOT NULL DEFAULT 0;
