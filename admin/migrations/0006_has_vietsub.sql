-- Denormalized "has a Vietnamese subtitle" flag so the viewer's browse cards
-- can badge movies without a per-row subquery. Maintained by AddSubtitle /
-- DeleteSubtitle; tracks title-level subtitles only (movie subs attach to
-- title_id; TV episode subs never set it).
ALTER TABLE titles ADD COLUMN has_vietsub TINYINT(1) NOT NULL DEFAULT 0;

-- Backfill; updated_at is preserved so the viewer's newest-first ordering
-- doesn't reshuffle.
UPDATE titles t
SET t.has_vietsub = 1, t.updated_at = t.updated_at
WHERE EXISTS (SELECT 1 FROM subtitles s WHERE s.title_id = t.id AND s.language = 'vi');
