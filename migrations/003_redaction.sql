-- Redaction records (notes/redaction.md). Counts only: never a matched value.
--
-- generations.redaction_rules/redactions: what the device reported for the
-- generation (the rule set it redacted with before chunking, and secrets
-- masked per rule). NULL rules: the device did not redact.
--
-- source_parse_state.server_redactions: secrets the server's own pass
-- masked while parsing the source's latest generation, per rule. Nonzero
-- means bytes reached the server unredacted (an old agent, the pre-redaction
-- archive, or a rule newer than the upload).
ALTER TABLE generations
  ADD COLUMN redaction_rules text,
  ADD COLUMN redactions jsonb;

ALTER TABLE source_parse_state
  ADD COLUMN server_redactions jsonb NOT NULL DEFAULT '{}';
