-- Paid 4K unlock, settled in crypto.
--
-- Two products hang off one ledger: a time-based pass (all 4K for N days) and a
-- permanent per-title unlock. The pass deliberately gets NO table of its own --
-- it materializes onto users.plan / users.plan_expires_at, the seam 0007
-- reserved for exactly this, which UserByID already selects. That keeps the
-- common "may this visitor play 4K" check free of an extra query per request.
--
-- HOW A PAYMENT IS IDENTIFIED. There is one receive address per chain family,
-- not one per invoice, and an invoice instead reserves a UNIQUE EXACT AMOUNT
-- (the price plus a little dust in the token's smallest decimals). The viewer
-- therefore holds no key material of any kind -- not even an xpub -- so a leaked
-- .env cannot move funds. It also means one 0x address serves every EVM chain,
-- with no per-address sweeping and no gas spent collecting dust.
--
-- lock_ns + amount_lock are that reservation. While an invoice is pending both
-- are set and the unique key stops any other open invoice in the same namespace
-- reserving the same amount, which is what makes a shared address safe. On
-- settle OR expiry both go back to NULL, and MySQL unique indexes allow
-- unlimited NULLs, so the amount is released for reuse. Every open EVM invoice
-- shares the single 'evm' namespace on purpose: no two can collide on ANY EVM
-- chain, so a user may pay on whichever chain is cheapest and still be credited
-- (paid_chain records where it actually landed, which may differ from chain).
--
-- DECIMAL(36,18) because token decimals differ per chain and per token -- USDT
-- is 6 decimals on Ethereum and Tron but 18 on BSC. 18 is the widest case.
--
-- NOTE the migrator in admin/store.go splits each file on the semicolon
-- character, so avoid using one anywhere except to terminate a statement (in
-- particular, not inside comments).

CREATE TABLE IF NOT EXISTS payment_invoices (
    id               BIGINT        AUTO_INCREMENT PRIMARY KEY,
    ref              CHAR(32)      NOT NULL,
    user_id          BIGINT        NOT NULL,
    kind             VARCHAR(16)   NOT NULL,
    plan_code        VARCHAR(32)   NOT NULL DEFAULT '',
    title_id         BIGINT        NULL,
    amount_usd_cents INT           NOT NULL,
    chain            VARCHAR(24)   NOT NULL,
    pay_to           VARCHAR(255)  NOT NULL,
    pay_amount       DECIMAL(36,18) NOT NULL,
    token            VARCHAR(16)   NOT NULL DEFAULT '',
    rate_usd         DECIMAL(32,12) NOT NULL DEFAULT 1,
    status           VARCHAR(16)   NOT NULL DEFAULT 'pending',
    received         DECIMAL(36,18) NOT NULL DEFAULT 0,
    paid_chain       VARCHAR(24)   NOT NULL DEFAULT '',
    confirmations    INT           NOT NULL DEFAULT 0,
    tx_hash          VARCHAR(128)  NOT NULL DEFAULT '',
    lock_ns          VARCHAR(16)   NULL,
    amount_lock      DECIMAL(36,18) NULL,
    expires_at       DATETIME      NOT NULL,
    paid_at          DATETIME      NULL,
    created_at       TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_invoice_ref (ref),
    UNIQUE KEY uniq_amount_lock (lock_ns, amount_lock),
    KEY idx_invoice_user (user_id, created_at),
    KEY idx_invoice_poll (status, expires_at),
    CONSTRAINT fk_inv_user  FOREIGN KEY (user_id)  REFERENCES users (id)  ON DELETE CASCADE,
    CONSTRAINT fk_inv_title FOREIGN KEY (title_id) REFERENCES titles (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Permanent per-title unlocks. (user_id, title_id) is unique so granting the
-- same purchase twice -- a duplicate scan observation, a restart mid-settle, a
-- rewound cursor -- is a harmless no-op rather than a double grant. The invoice
-- FK is SET NULL rather than CASCADE: losing the receipt must never silently
-- revoke something the user paid for.

CREATE TABLE IF NOT EXISTS user_title_unlocks (
    id         BIGINT    AUTO_INCREMENT PRIMARY KEY,
    user_id    BIGINT    NOT NULL,
    title_id   BIGINT    NOT NULL,
    invoice_id BIGINT    NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_title_unlock (user_id, title_id),
    KEY idx_unlock_user (user_id),
    CONSTRAINT fk_utu_user    FOREIGN KEY (user_id)    REFERENCES users (id)             ON DELETE CASCADE,
    CONSTRAINT fk_utu_title   FOREIGN KEY (title_id)   REFERENCES titles (id)            ON DELETE CASCADE,
    CONSTRAINT fk_utu_invoice FOREIGN KEY (invoice_id) REFERENCES payment_invoices (id)  ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Where each chain's scan got to, so a restart resumes instead of rescanning
-- from genesis. It is a block number on EVM chains and a millisecond timestamp
-- on Tron, hence a string rather than a number.
--
-- The column is scan_cursor, NOT cursor: CURSOR is a reserved word in BOTH
-- MySQL 8 and the MariaDB used in production (stored-procedure syntax), so
-- unquoted it is a syntax error and quoted it would need backquotes forever.

CREATE TABLE IF NOT EXISTS billing_chain_cursors (
    chain       VARCHAR(24)  NOT NULL PRIMARY KEY,
    scan_cursor VARCHAR(128) NOT NULL DEFAULT '',
    updated_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
