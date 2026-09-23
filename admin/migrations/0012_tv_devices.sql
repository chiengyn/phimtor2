-- Paired televisions, for the Android TV client.
--
-- A TV cannot run the Google redirect flow: there is no keyboard worth typing a
-- password on and no browser to hand the callback to. So it pairs instead --
-- the TV shows a short code, the user approves it on a phone that is already
-- signed in, and the TV exchanges its long code for a bearer token. This table
-- is the record of that exchange.
--
-- Like users and user_bookmarks from 0007, this service DECLARES the table and
-- never writes it. The viewer is the sole writer, because pairing is an account
-- action and accounts live entirely on the viewer side. Deploy admin before
-- viewer, as always.
--
-- Why a table at all, when a session is famously a signed cookie with no
-- storage behind it (see 0007 and viewer/session.go): a browser cookie belongs
-- to one person who can clear it, whereas a television is furniture in a shared
-- room and may outlive the account holder's interest in it. Rotating
-- SESSION_SECRET -- the only revocation lever a stateless token has -- would
-- sign every user out on every device to unpair one TV. The row makes "sign out
-- this television" a single UPDATE, and it is the reason the bearer payload
-- carries a device id the session cookie has no equivalent of.
--
-- TWO codes, with different jobs and therefore different shapes:
--
--   device_code  the long, secret, crypto-random one. Only the TV ever sees it,
--                it travels over TLS, and it is what the TV polls with. Unique
--                forever -- it is a credential.
--   user_code    the short, human one, read off a screen across a room and typed
--                into a phone. Necessarily low-entropy, hence rate limiting in
--                the handler, a short expiry, and the NULLing below.
--
-- user_code is UNIQUE but NULLABLE, and the viewer NULLs it the moment a row
-- leaves 'pending'. MySQL treats NULLs as DISTINCT in a unique index -- the same
-- property 0011 notes and works around -- which here is exactly what we want: it
-- guarantees no two OPEN pairings can show the same code, while letting a code
-- be handed out again once the pairing it belonged to is settled. A plain
-- UNIQUE that kept the value would burn one code out of a small alphabet per
-- pairing, forever.
--
-- ON DELETE CASCADE, unlike the SET NULL in 0011: a subtitle outlives its
-- contributor because other people are watching it, but a pairing has no
-- meaning without the account it grants access to. Deleting the user must
-- delete the grant.
--
-- NOTE the migrator in admin/store.go splits each file on the semicolon
-- character, so avoid using one anywhere except to terminate a statement (in
-- particular, not inside comments).

CREATE TABLE IF NOT EXISTS tv_devices (
    id           BIGINT       AUTO_INCREMENT PRIMARY KEY,
    device_code  VARCHAR(64)  NOT NULL,
    user_code    VARCHAR(16)  NULL,
    user_id      BIGINT       NULL,
    device_name  VARCHAR(128) NOT NULL DEFAULT '',
    status       VARCHAR(16)  NOT NULL DEFAULT 'pending',
    created_at   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at   DATETIME     NOT NULL,
    approved_at  DATETIME     NULL,
    last_seen_at DATETIME     NULL,
    UNIQUE KEY uq_tv_device_code (device_code),
    UNIQUE KEY uq_tv_user_code (user_code),
    KEY idx_tv_device_user (user_id),
    KEY idx_tv_device_pending (status, expires_at),
    CONSTRAINT fk_tv_device_user
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
