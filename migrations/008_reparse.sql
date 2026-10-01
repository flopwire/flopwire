-- NULL stamps identify a source that has not completed a versioned parse.
ALTER TABLE source_parse_state ADD COLUMN applied_parser text;
ALTER TABLE source_parse_state ADD COLUMN applied_redaction_rules text;
ALTER TABLE source_parse_state ADD COLUMN refresh_requested_at timestamptz;
-- Sources a read prioritized, which the idle refresh picks first.
CREATE INDEX source_parse_refresh_idx ON source_parse_state (refresh_requested_at)
  WHERE refresh_requested_at IS NOT NULL;
ALTER TABLE messages ADD COLUMN redaction_rules text;
