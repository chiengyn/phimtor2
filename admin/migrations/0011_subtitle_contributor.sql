-- Attribution for user-contributed subtitles.
--
-- Until now `subtitles` had exactly one writer: this service (the admin UI and
-- the crawler). The public viewer now lets a SIGNED-IN visitor search a
-- provider and save the result for everyone, so the table gains a second
-- writer. The two are kept apart by ROWS, not by columns:
--
--     added_by_user_id IS NULL      -- admin-curated (this service, as before)
--     added_by_user_id = <user id>  -- contributed by that viewer account
--
-- That differs from the users-table split in 0009, where the two services own
-- disjoint COLUMNS of one shared row. Here each row has exactly one author, so
-- a single nullable column records it and nothing is shared. The admin never
-- writes this column; the viewer always does.
--
-- Why this instead of a separate user_subtitles table: the watch page fetches a
-- subtitle by id (GET /api/subtitles/{id}/file), and two tables would mean two
-- auto-increment sequences whose ids collide -- forcing a second file endpoint,
-- a merge in every list query, a UNION in the has_vietsub recompute, and new
-- admin screens to moderate rows the existing UI cannot see. One nullable
-- column leaves every read path untouched, and the existing
-- DELETE /api/subtitles/{id} moderates a contributed row for free.
--
-- ON DELETE SET NULL, not CASCADE: deleting an account must not delete
-- subtitles other people are watching. The row survives and loses only its
-- attribution.
--
-- The column, its index and its foreign key are added in ONE statement for the
-- reason spelled out in 0009: the migrator records a file as applied only after
-- every statement in it succeeds, and MySQL 8 has no ADD COLUMN IF NOT EXISTS,
-- so a half-applied set would need hand surgery to recover.
--
-- NOTE the migrator in admin/store.go splits each file on the semicolon
-- character, so avoid using one anywhere except to terminate a statement (in
-- particular, not inside comments).

ALTER TABLE subtitles
    ADD COLUMN added_by_user_id BIGINT NULL,
    ADD KEY idx_subtitle_user (added_by_user_id),
    ADD CONSTRAINT fk_subtitle_user
        FOREIGN KEY (added_by_user_id) REFERENCES users (id) ON DELETE SET NULL;
