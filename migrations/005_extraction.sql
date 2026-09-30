-- Reports describe the extraction that produced each checkpoint. NULL means
-- unassessed; old cursors are reparsed by the queue's contract sweep.
ALTER TABLE source_parse_state ADD COLUMN extraction_report jsonb;

-- A full parse retires rows it did not emit, even in the same raw generation.
ALTER TABLE messages ADD COLUMN parse_attempt bigint NOT NULL DEFAULT 0;
CREATE SEQUENCE extraction_attempt_seq;
