-- Admin-granted complimentary 4K (a "comp"), separate from the paid pass.
--
-- CORRECTION to the header of 0007_users.sql: that file states the admin never
-- writes `users`. As of this migration the admin writes exactly TWO columns on
-- that table -- the two added here -- and nothing else, ever. 0007 is already
-- applied so its comment cannot be edited, which is why the correction lives
-- here.
--
-- Why separate columns instead of reusing plan / plan_expires_at: those belong
-- to the viewer's billing path, which extends plan_expires_at when an invoice
-- settles. Keeping the comp in its own columns makes the two writers DISJOINT --
-- admin touches comp_*, viewer touches plan* -- so revoking a comp provably
-- cannot cancel a subscription the user paid for (the UPDATE never names those
-- columns), and a comped account stays distinguishable from real revenue.
--
-- comp_expires_at NULL means "no grant". A PERMANENT grant is stored as the
-- maximum DATETIME (9999-12-31 23:59:59) rather than by adding a boolean, so the
-- entitlement predicate stays a single `comp_expires_at > NOW()` -- the same
-- shape as the paid check -- with no second condition to get wrong and no state
-- where two columns can disagree. Only the admin side knows that sentinel
-- exists: it writes it and renders it as "forever", while the viewer only ever
-- compares against now and never learns the concept.
--
-- No index: every access to these is by primary key.
--
-- Both columns are added in ONE statement on purpose. The migrator records a
-- file as applied only after every statement in it succeeds, and MySQL 8 has no
-- ADD COLUMN IF NOT EXISTS, so a half-applied pair would be unrecoverable
-- without hand surgery.
--
-- NOTE the migrator in admin/store.go splits each file on the semicolon
-- character, so avoid using one anywhere except to terminate a statement (in
-- particular, not inside comments).

ALTER TABLE users
    ADD COLUMN comp_expires_at DATETIME NULL,
    ADD COLUMN comp_granted_at DATETIME NULL;
