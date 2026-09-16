-- Localized catalog copy. The original columns remain as the Vietnamese-first
-- compatibility fallback while the catalog is backfilled.

CREATE TABLE IF NOT EXISTS title_translations (
    title_id      BIGINT       NOT NULL,
    locale        VARCHAR(16)  NOT NULL,
    title         VARCHAR(512) NOT NULL DEFAULT '',
    overview      TEXT         NULL,
    poster_path   VARCHAR(255) NULL,
    backdrop_path VARCHAR(255) NULL,
    PRIMARY KEY (title_id, locale),
    KEY idx_title_translation_locale_title (locale, title),
    CONSTRAINT fk_title_translation_title FOREIGN KEY (title_id) REFERENCES titles (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS genre_translations (
    genre_id INT          NOT NULL,
    locale   VARCHAR(16)  NOT NULL,
    name     VARCHAR(128) NOT NULL DEFAULT '',
    PRIMARY KEY (genre_id, locale),
    KEY idx_genre_translation_locale_name (locale, name),
    CONSTRAINT fk_genre_translation_genre FOREIGN KEY (genre_id) REFERENCES genres (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS season_translations (
    season_id   BIGINT       NOT NULL,
    locale      VARCHAR(16)  NOT NULL,
    name        VARCHAR(512) NOT NULL DEFAULT '',
    overview    TEXT         NULL,
    poster_path VARCHAR(255) NULL,
    PRIMARY KEY (season_id, locale),
    CONSTRAINT fk_season_translation_season FOREIGN KEY (season_id) REFERENCES seasons (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS episode_translations (
    episode_id BIGINT       NOT NULL,
    locale     VARCHAR(16)  NOT NULL,
    name       VARCHAR(512) NOT NULL DEFAULT '',
    overview   TEXT         NULL,
    PRIMARY KEY (episode_id, locale),
    CONSTRAINT fk_episode_translation_episode FOREIGN KEY (episode_id) REFERENCES episodes (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
