-- Public viewer accounts, and the watch-later list that now belongs to them.
--
-- Sign-in is delegated to an OIDC provider (Google today), so no password or
-- credential material ever lives here. The identity is (provider, provider_uid)
-- rather than the email address, so a user who changes their Gmail address keeps
-- the same row and the same saved list. Email is descriptive metadata only and
-- is deliberately NOT unique.
--
-- A session is a signed cookie carrying users.id, so there is no sessions table
-- on purpose. The plan / plan_expires_at pair is the seam for the future paid
-- unlock -- everyone is 'free' and nothing reads them yet, but a later
-- entitlements table can hang off users.id without touching this one.
--
-- NOTE the migrator in admin/store.go splits each file on the semicolon
-- character, so avoid using one anywhere except to terminate a statement (in
-- particular, not inside comments).

CREATE TABLE IF NOT EXISTS users (
    id              BIGINT        AUTO_INCREMENT PRIMARY KEY,
    provider        VARCHAR(32)   NOT NULL DEFAULT 'google',
    provider_uid    VARCHAR(255)  NOT NULL,
    email           VARCHAR(320)  NOT NULL DEFAULT '',
    email_verified  TINYINT(1)    NOT NULL DEFAULT 0,
    name            VARCHAR(255)  NOT NULL DEFAULT '',
    avatar_url      VARCHAR(1024) NOT NULL DEFAULT '',
    locale          VARCHAR(16)   NOT NULL DEFAULT '',
    plan            VARCHAR(32)   NOT NULL DEFAULT 'free',
    plan_expires_at DATETIME      NULL,
    is_blocked      TINYINT(1)    NOT NULL DEFAULT 0,
    last_login_at   DATETIME      NULL,
    created_at      TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_user_identity (provider, provider_uid),
    KEY idx_user_email (email),
    KEY idx_user_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- The signed-in watch-later list, replacing the per-browser localStorage list
-- (which stays the fallback for anonymous visitors). Only the title id is
-- stored -- the card is re-rendered from `titles` by the same server-side
-- partial the grid uses, so a saved entry can never show stale metadata, and a
-- deleted title cascades out of every list instead of leaving a dead card.
--
-- (user_id, title_id) is unique so a double POST is a harmless no-op, and the
-- composite (user_id, created_at) index serves the newest-first list read.

CREATE TABLE IF NOT EXISTS user_bookmarks (
    id         BIGINT    AUTO_INCREMENT PRIMARY KEY,
    user_id    BIGINT    NOT NULL,
    title_id   BIGINT    NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_user_bookmark (user_id, title_id),
    KEY idx_bookmark_user_created (user_id, created_at),
    CONSTRAINT fk_ub_user  FOREIGN KEY (user_id)  REFERENCES users (id)  ON DELETE CASCADE,
    CONSTRAINT fk_ub_title FOREIGN KEY (title_id) REFERENCES titles (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
