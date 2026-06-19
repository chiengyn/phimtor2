-- Torrents attached to a movie (title_id) or a TV episode (episode_id).
-- Exactly one of title_id / episode_id is set, enforced by chk_torrent_owner
-- and also guarded in the application layer (Store.AddTorrent).
-- The streamer service keeps no persistent state, so this table is the source
-- of truth: it holds a re-addable handle (always a magnet URI, plus the raw
-- .torrent bytes when the torrent was added by file upload).

CREATE TABLE IF NOT EXISTS torrents (
    id           BIGINT AUTO_INCREMENT PRIMARY KEY,
    title_id     BIGINT        NULL,
    episode_id   BIGINT        NULL,
    name         VARCHAR(512)  NOT NULL,
    resolution   ENUM('2160p','1080p','720p') NOT NULL,
    info_hash    CHAR(40)      NOT NULL,
    magnet       TEXT          NOT NULL,
    torrent_file LONGBLOB      NULL,
    file_index   INT           NOT NULL,
    file_path    VARCHAR(1024) NOT NULL,
    file_size    BIGINT        NOT NULL DEFAULT 0,
    created_at   TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    KEY idx_torrent_title (title_id),
    KEY idx_torrent_episode (episode_id),
    CONSTRAINT fk_torrent_title FOREIGN KEY (title_id) REFERENCES titles (id) ON DELETE CASCADE,
    CONSTRAINT fk_torrent_episode FOREIGN KEY (episode_id) REFERENCES episodes (id) ON DELETE CASCADE,
    CONSTRAINT chk_torrent_owner CHECK (
        (title_id IS NOT NULL AND episode_id IS NULL)
        OR (title_id IS NULL AND episode_id IS NOT NULL)
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
