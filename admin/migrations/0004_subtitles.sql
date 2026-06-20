-- Persisted subtitles attached to a movie title OR a single TV episode.
-- A subtitle is downloaded from a provider (OpenSubtitles today, extensible to
-- others via the `provider` column) and the file itself is stored in a BlobStore
-- (local disk or S3-compatible object storage). Only the locator is kept here:
--   storage_backend : which BlobStore holds the file ('local' | 's3')
--   storage_key     : the key/path of the file within that backend
-- Provider-specific extras beyond the typed columns live in `metadata` (JSON).
-- Mirrors the videos owner model: exactly one of title_id / episode_id is set,
-- and many subtitles (languages/releases) may exist per owner.
-- The migrator splits on the semicolon character, so avoid semicolons except to
-- terminate a statement (in particular, not inside comments).

CREATE TABLE IF NOT EXISTS subtitles (
    id               BIGINT AUTO_INCREMENT PRIMARY KEY,
    title_id         BIGINT        NULL,
    episode_id       BIGINT        NULL,
    provider         VARCHAR(32)   NOT NULL DEFAULT 'opensubtitles',
    provider_file_id VARCHAR(128)  NOT NULL,
    language         VARCHAR(16)   NOT NULL,
    name             VARCHAR(512)  NOT NULL,
    download_count   INT           NOT NULL DEFAULT 0,
    format           VARCHAR(8)    NOT NULL DEFAULT 'vtt',
    storage_backend  VARCHAR(16)   NOT NULL,
    storage_key      VARCHAR(1024) NOT NULL,
    metadata         JSON          NULL,
    created_at       TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    KEY idx_subtitle_title (title_id),
    KEY idx_subtitle_episode (episode_id),
    CONSTRAINT fk_subtitle_title FOREIGN KEY (title_id) REFERENCES titles (id) ON DELETE CASCADE,
    CONSTRAINT fk_subtitle_episode FOREIGN KEY (episode_id) REFERENCES episodes (id) ON DELETE CASCADE,
    CONSTRAINT chk_subtitle_owner CHECK (
        (title_id IS NOT NULL AND episode_id IS NULL)
        OR (title_id IS NULL AND episode_id IS NOT NULL)
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
