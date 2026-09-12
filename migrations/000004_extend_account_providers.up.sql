-- nexusmail:foreign_keys=off
--
-- Extend accounts.provider to admit 126 and icloud.
--
-- SQLite cannot alter a CHECK constraint, so the only way to widen the set is
-- the documented 12-step table rebuild. accounts is the parent of three
-- ON DELETE CASCADE chains (mailboxes, messages, drafts), and with foreign keys
-- enabled DROP TABLE performs an implicit DELETE FROM that fires those actions.
-- Run inside the normal migration transaction, both outcomes were measured on
-- this exact schema:
--
--   * with mailboxes and drafts but no mail, the migration SUCCEEDS and both
--     tables come out empty. No error, no log line, no way to notice.
--   * with mail present, the cascade into messages fires the message_fts delete
--     trigger, which collides with the in-flight DROP and fails the migration
--     with "database table is locked".
--
-- The first is the dangerous one, and it is why this is a directive rather than a
-- comment: nothing downstream would have reported it.
--
-- PRAGMA foreign_keys is a no-op inside a transaction and defer_foreign_keys
-- only postpones violation checks, not the actions, so neither can be used here.
-- The directive on line 1 makes the runner disable foreign keys on the migration
-- connection *before* it opens the transaction, and re-enable them plus run
-- foreign_key_check after the commit. Renaming the old table first instead does
-- not help: with foreign keys on, ALTER TABLE RENAME rewrites the children's
-- REFERENCES clauses to follow it, and PRAGMA legacy_alter_table does not cover
-- that rewrite.
--
-- The column list is copied verbatim from 000001_init, including every other
-- CHECK and the UNIQUE (provider, email) index, because a rebuild silently drops
-- whatever it does not restate. The INSERT names its columns rather than using
-- SELECT *, so a future column added to accounts fails this migration loudly
-- instead of shifting values into the wrong columns.
--
-- id values are carried over unchanged, which keeps the children's account_id
-- valid and leaves sqlite_sequence at max(id).

CREATE TABLE accounts_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email TEXT NOT NULL COLLATE NOCASE,
    display_name TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL CHECK (provider IN ('qq', '163', '126', 'gmail', 'outlook', 'icloud')),
    auth_type TEXT NOT NULL CHECK (auth_type IN ('password', 'oauth2')),
    username TEXT NOT NULL,
    imap_host TEXT NOT NULL,
    imap_port INTEGER NOT NULL DEFAULT 993 CHECK (imap_port BETWEEN 1 AND 65535),
    imap_tls_mode TEXT NOT NULL DEFAULT 'implicit' CHECK (imap_tls_mode IN ('implicit', 'starttls')),
    smtp_host TEXT NOT NULL,
    smtp_port INTEGER NOT NULL CHECK (smtp_port BETWEEN 1 AND 65535),
    smtp_tls_mode TEXT NOT NULL CHECK (smtp_tls_mode IN ('implicit', 'starttls')),
    secret_ciphertext BLOB NOT NULL,
    status TEXT NOT NULL DEFAULT 'disconnected' CHECK (status IN (
        'disconnected', 'connecting', 'syncing', 'connected', 'backoff', 'auth_error', 'error'
    )),
    last_error TEXT,
    last_connected_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (provider, email)
);

INSERT INTO accounts_new (
    id, email, display_name, provider, auth_type, username,
    imap_host, imap_port, imap_tls_mode, smtp_host, smtp_port, smtp_tls_mode,
    secret_ciphertext, status, last_error, last_connected_at, created_at, updated_at
)
SELECT
    id, email, display_name, provider, auth_type, username,
    imap_host, imap_port, imap_tls_mode, smtp_host, smtp_port, smtp_tls_mode,
    secret_ciphertext, status, last_error, last_connected_at, created_at, updated_at
FROM accounts;

DROP TABLE accounts;

ALTER TABLE accounts_new RENAME TO accounts;
