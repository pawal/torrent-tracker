-- Retention sweeps the lookup log by timestamp across every tracker, which the
-- per-tracker index cannot serve.
CREATE INDEX lookups_ts ON lookups (ts);
