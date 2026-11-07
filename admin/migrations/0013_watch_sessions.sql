-- Watch history: which signed-in user watched which title, when, and for how
-- long. One row per watch SESSION -- one player page load on one source -- so a
-- viewer who switches from 1080p to 4K mid-film gets two rows.
--
-- Like users (0007) and tv_devices (0012), this service DECLARES the table and
-- never writes it. The viewer is the sole writer: it builds each session in
-- memory from the watch page's existing 10-second heartbeat and flushes the
-- running totals here on an interval. The admin only reads it, at
-- GET /watch-history. Deploy admin before viewer, as always.
--
-- The viewer UPSERTS rather than appends. It holds each session's CUMULATIVE
-- totals in memory and writes them on every flush, keyed by the unique
-- (user_id, session_id, video_id) below, so a flush that fails is simply
-- retried with the same numbers and can never double-count.
--
-- watched_seconds is how long the PLAYER WAS OPEN, not how long the video
-- played: the heartbeat keeps beating while paused. Gaps between beats are
-- capped by the viewer, so a laptop asleep on the watch page does not count.
--
-- title_id is denormalised (an episode's title is reachable through
-- episodes -> seasons) so per-title reporting needs no join, and it is NOT NULL
-- because every video belongs to a title one way or the other. video_id and
-- episode_id are SET NULL on delete: history should outlive an admin replacing
-- a torrent or re-importing an episode. Deleting the user or the title deletes
-- their history.
--
-- NOTE the migrator in admin/store.go splits each file on the semicolon
-- character, so avoid using one anywhere except to terminate a statement (in
-- particular, not inside comments).

CREATE TABLE IF NOT EXISTS watch_sessions (
    id              BIGINT      AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT      NOT NULL,
    session_id      VARCHAR(64) NOT NULL,
    video_id        BIGINT      NULL,
    title_id        BIGINT      NOT NULL,
    episode_id      BIGINT      NULL,
    resolution      VARCHAR(16) NOT NULL DEFAULT '',
    client          VARCHAR(8)  NOT NULL DEFAULT 'web',
    started_at      DATETIME    NOT NULL,
    last_seen_at    DATETIME    NOT NULL,
    watched_seconds INT         NOT NULL DEFAULT 0,
    UNIQUE KEY uq_watch_session (user_id, session_id, video_id),
    KEY idx_ws_started (started_at),
    KEY idx_ws_user_started (user_id, started_at),
    KEY idx_ws_title_started (title_id, started_at),
    CONSTRAINT fk_ws_user    FOREIGN KEY (user_id)    REFERENCES users (id)    ON DELETE CASCADE,
    CONSTRAINT fk_ws_title   FOREIGN KEY (title_id)   REFERENCES titles (id)   ON DELETE CASCADE,
    CONSTRAINT fk_ws_episode FOREIGN KEY (episode_id) REFERENCES episodes (id) ON DELETE SET NULL,
    CONSTRAINT fk_ws_video   FOREIGN KEY (video_id)   REFERENCES videos (id)   ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
