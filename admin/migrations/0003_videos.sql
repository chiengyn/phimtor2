-- Season-pack support. Replace the single `torrents` table with two:
--   torrent_sources : one row per .torrent (keyed by info_hash), holding the
--                     magnet and the raw .torrent bytes shared by every file.
--   videos          : one row per playable file. A video belongs to exactly one
--                     owner (movie title OR episode) and points at one source
--                     plus a file_index inside it.
-- Relationships: title 1-n videos, episode 1-n videos, videos n-1 torrent_sources.
-- A season pack is one source with many video rows (one per episode file).
-- The migrator splits on the semicolon character, so avoid semicolons except to
-- terminate a statement (in particular, not inside comments).

CREATE TABLE IF NOT EXISTS torrent_sources (
    id           BIGINT AUTO_INCREMENT PRIMARY KEY,
    info_hash    CHAR(40)  NOT NULL,
    magnet       TEXT      NOT NULL,
    torrent_file LONGBLOB  NULL,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_info_hash (info_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS videos (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    source_id   BIGINT        NOT NULL,
    title_id    BIGINT        NULL,
    episode_id  BIGINT        NULL,
    name        VARCHAR(512)  NOT NULL,
    resolution  ENUM('2160p','1080p','720p') NOT NULL,
    file_index  INT           NOT NULL,
    file_path   VARCHAR(1024) NOT NULL,
    file_size   BIGINT        NOT NULL DEFAULT 0,
    created_at  TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    KEY idx_video_source (source_id),
    KEY idx_video_title (title_id),
    KEY idx_video_episode (episode_id),
    CONSTRAINT fk_video_source FOREIGN KEY (source_id) REFERENCES torrent_sources (id) ON DELETE CASCADE,
    CONSTRAINT fk_video_title FOREIGN KEY (title_id) REFERENCES titles (id) ON DELETE CASCADE,
    CONSTRAINT fk_video_episode FOREIGN KEY (episode_id) REFERENCES episodes (id) ON DELETE CASCADE,
    CONSTRAINT chk_video_owner CHECK (
        (title_id IS NOT NULL AND episode_id IS NULL)
        OR (title_id IS NULL AND episode_id IS NOT NULL)
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS torrents;
