ALTER TABLE sources
  ADD COLUMN antigravity_parent_session_id text,
  ADD COLUMN antigravity_depth integer,
  ADD COLUMN antigravity_lineage_observed_at bigint;
