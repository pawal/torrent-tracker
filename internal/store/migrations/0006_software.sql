-- What software answered. No tracker discloses a version, but its failure text
-- and reply shape are literals in the implementation, so they cluster the
-- registry from the body the prober already fetched. The raw signature is
-- stored, not a name for it: a guess baked into history cannot be corrected.
ALTER TABLE probes ADD COLUMN signature TEXT NOT NULL DEFAULT '';

-- Names the front end, not the tracker; nginx and Cloudflare overwrite it.
ALTER TABLE probes ADD COLUMN server TEXT NOT NULL DEFAULT '';

CREATE INDEX probes_signature ON probes (signature) WHERE signature != '';
