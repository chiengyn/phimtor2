-- TMDB admin schema. utf8mb4 throughout so Vietnamese text round-trips.
-- The migrator splits each file on the semicolon character and runs every
-- statement individually, so avoid using a semicolon anywhere except to
-- terminate a statement (in particular, not inside comments).

CREATE TABLE IF NOT EXISTS titles (
    id                BIGINT AUTO_INCREMENT PRIMARY KEY,
    tmdb_id           INT          NOT NULL,
    type              ENUM('movie','tv') NOT NULL,
    title             VARCHAR(512) NOT NULL,
    original_title    VARCHAR(512) NOT NULL DEFAULT '',
    overview          TEXT         NULL,
    air_date          DATE         NULL,
    runtime           INT          NULL,
    poster_path       VARCHAR(255) NULL,
    backdrop_path     VARCHAR(255) NULL,
    vote_average      DECIMAL(4,2) NULL,
    original_language VARCHAR(16)  NULL,
    status            VARCHAR(64)  NULL,
    created_at        TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_tmdb (tmdb_id, type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS genres (
    id   INT          NOT NULL PRIMARY KEY,
    name VARCHAR(128) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS title_genres (
    title_id BIGINT NOT NULL,
    genre_id INT    NOT NULL,
    PRIMARY KEY (title_id, genre_id),
    CONSTRAINT fk_tg_title FOREIGN KEY (title_id) REFERENCES titles (id) ON DELETE CASCADE,
    CONSTRAINT fk_tg_genre FOREIGN KEY (genre_id) REFERENCES genres (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS seasons (
    id            BIGINT AUTO_INCREMENT PRIMARY KEY,
    title_id      BIGINT       NOT NULL,
    season_number INT          NOT NULL,
    name          VARCHAR(512) NOT NULL DEFAULT '',
    overview      TEXT         NULL,
    air_date      DATE         NULL,
    poster_path   VARCHAR(255) NULL,
    UNIQUE KEY uniq_season (title_id, season_number),
    CONSTRAINT fk_season_title FOREIGN KEY (title_id) REFERENCES titles (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS episodes (
    id             BIGINT AUTO_INCREMENT PRIMARY KEY,
    season_id      BIGINT       NOT NULL,
    episode_number INT          NOT NULL,
    name           VARCHAR(512) NOT NULL DEFAULT '',
    overview       TEXT         NULL,
    air_date       DATE         NULL,
    runtime        INT          NULL,
    still_path     VARCHAR(255) NULL,
    UNIQUE KEY uniq_episode (season_id, episode_number),
    CONSTRAINT fk_episode_season FOREIGN KEY (season_id) REFERENCES seasons (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
