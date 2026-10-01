-- NULL stamps identify a source that has not completed a versioned parse.
ALTER TABLE source_parse_state ADD COLUMN applied_parser text;
ALTER TABLE source_parse_state ADD COLUMN applied_redaction_rules text;
ALTER TABLE source_parse_state ADD COLUMN refresh_requested_at timestamptz;
ALTER TABLE messages ADD COLUMN redaction_rules text;
