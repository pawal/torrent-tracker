-- An endpoint the host itself says it does not serve: BEP 34 advertises the
-- port, and neither http nor https answers there. The row stays so its probe
-- history does; retiring only stops the probing and the listing.
ALTER TABLE endpoints ADD COLUMN retired_at TEXT;
