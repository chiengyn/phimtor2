-- The ad network script and its on/off switch, edited at GET /ads. One row, id 1.
-- The admin is the only writer. The viewer reads it on an interval and prints
-- script into its public pages, unescaped, while enabled is 1.
-- Seeded off and empty, so this migration alone changes nothing on the site.
CREATE TABLE IF NOT EXISTS ad_settings (
    id         TINYINT    NOT NULL PRIMARY KEY,
    enabled    TINYINT(1) NOT NULL DEFAULT 0,
    script     TEXT       NOT NULL,
    updated_at TIMESTAMP  NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    CONSTRAINT chk_ad_settings_single_row CHECK (id = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT IGNORE INTO ad_settings (id, enabled, script) VALUES (1, 0, '');
