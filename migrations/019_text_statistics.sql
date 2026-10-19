-- A scoped grep (repo, session, device) narrows to a conversation cohort,
-- but the planner only intersects the trigram bitmap with the conversation
-- index when it expects the literal to be common. With the default 100-bucket
-- histogram on text, a literal matching 1.7% of 2.8M rows is estimated at 273
-- rows (actual 47,966), so the plan reads 32k heap pages to keep 1,333 and the
-- first candidate page takes 13-17 s (#291). At 1000 buckets the estimate is
-- close enough for a BitmapAnd with messages_default_filter_idx: 2.8k pages,
-- 1.6 s on the same data. Rare literals keep their trigram-only plan.
-- ANALYZE so the target takes effect at once (about a minute on a few
-- million messages). An installation that already set and analyzed the
-- target by hand, as production did on 2026-10-19, skips both.
DO $$
BEGIN
  IF (SELECT attstattarget FROM pg_attribute WHERE attrelid='messages'::regclass AND attname='text') IS DISTINCT FROM 1000 THEN
    ALTER TABLE messages ALTER COLUMN text SET STATISTICS 1000;
    ANALYZE messages;
  END IF;
END
$$;
