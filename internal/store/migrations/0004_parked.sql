-- Parked names. A control name is one known not to be a tracker, so whatever
-- it resolves to is a parking answer by definition. Any tracker whose entire
-- address set is parking answers is parked rather than alive.
ALTER TABLE trackers ADD COLUMN control INTEGER NOT NULL DEFAULT 0;
ALTER TABLE trackers ADD COLUMN parked  INTEGER NOT NULL DEFAULT 0;

-- The seed list has carried this name since 2012 for exactly this purpose: it
-- is meant never to resolve. It does now, and so do 26 dead trackers alongside
-- it, all on one parking host.
UPDATE trackers SET control = 1 WHERE name = '0123456789nonexistent.com';
