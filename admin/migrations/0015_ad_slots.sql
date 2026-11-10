-- Banner ad slots, edited at GET /ads next to the global script in ad_settings.
-- One row per slot, keyed by the names in admin/ads.go adSlotDefs (mirrored in
-- viewer/ads.go adSlotKeys). The admin is the only writer. The viewer reads the
-- enabled rows on an interval and prints the code unescaped where the slot sits.
-- code is the desktop banner. mobile_code, when set, replaces it on narrow
-- screens, picked in the browser by screen width. Empty means phones get code.
-- ad_settings.enabled stays the master switch: while it is 0 no slot shows.
-- Rows appear on the first save, so this migration alone changes nothing.
CREATE TABLE IF NOT EXISTS ad_slots (
    slot        VARCHAR(32) NOT NULL PRIMARY KEY,
    enabled     TINYINT(1)  NOT NULL DEFAULT 0,
    code        TEXT        NOT NULL,
    mobile_code TEXT        NOT NULL,
    updated_at  TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
