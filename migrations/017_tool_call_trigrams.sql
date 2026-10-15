-- Exclude other message kinds before trigram heap/TOAST rechecks. Existing
-- installations can build this index CONCURRENTLY before the migration.
-- Accept that prebuild only when its complete definition is the expected one.
DO $$
DECLARE
  target oid := 'messages'::regclass;
  target_schema text;
  trgm_schema text;
  trgm_class oid;
  existing oid;
  compatible boolean;
BEGIN
  SELECT n.nspname INTO STRICT target_schema
    FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
   WHERE c.oid=target;
  SELECT n.nspname, o.oid INTO STRICT trgm_schema, trgm_class
    FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace
    JOIN pg_opclass o ON o.opcnamespace=n.oid AND o.opcname='gin_trgm_ops'
    JOIN pg_am a ON a.oid=o.opcmethod AND a.amname='gin'
   WHERE e.extname='pg_trgm';
  existing := to_regclass(format('%I.messages_tool_call_trgm_idx', target_schema));
  IF existing IS NULL THEN
    EXECUTE format('CREATE INDEX messages_tool_call_trgm_idx ON %I.messages USING gin (text %I.gin_trgm_ops) WHERE kind = ''tool_call''', target_schema, trgm_schema);
  ELSE
    SELECT i.indrelid=target AND a.amname='gin'
       AND i.indisvalid AND i.indisready AND i.indislive
       AND NOT i.indisunique AND NOT i.indisprimary AND NOT i.indisexclusion
       AND i.indnkeyatts=1 AND i.indnatts=1
       AND i.indkey[0]=t.attnum AND i.indclass[0]=trgm_class
       AND i.indcollation[0]=t.attcollation AND i.indoption[0]=0
       AND i.indexprs IS NULL AND c.reloptions IS NULL
       AND pg_get_expr(i.indpred,i.indrelid)='(kind = ''tool_call''::text)'
      INTO compatible
      FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
      JOIN pg_am a ON a.oid=c.relam
      JOIN pg_attribute t ON t.attrelid=target AND t.attname='text' AND NOT t.attisdropped
     WHERE i.indexrelid=existing;
    IF compatible IS DISTINCT FROM true THEN
      RAISE EXCEPTION 'existing messages_tool_call_trgm_idx is not the required valid tool_call trigram index';
    END IF;
  END IF;
END
$$;
