-- Which sort of evidence the signature is. A failure text is a literal lifted
-- from the implementation and identifies it; a reply's shape is only the keys it
-- happened to carry, and those come and go with the peers a tracker has to
-- report. Grouping the two alike split one implementation across a dozen rows.
--
-- Derived from the reply, not from the stored string: "missing info_hash" is a
-- failure text that reads exactly like a one-key shape. Rows probed before this
-- column existed keep an empty kind and group by their raw signature until the
-- next pass rewrites them.
ALTER TABLE probes ADD COLUMN signature_kind TEXT NOT NULL DEFAULT '';
