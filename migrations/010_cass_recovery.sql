-- A CASS recovery export is normalized archive evidence, distinct from a
-- native harness transcript. Add this format to existing installations.
ALTER TABLE sources DROP CONSTRAINT sources_storage_kind_check;
ALTER TABLE sources ADD CONSTRAINT sources_storage_kind_check
 CHECK (storage_kind IN ('jsonl_append','json_doc','sqlite','dir','markdown','companion','cass_export'));
