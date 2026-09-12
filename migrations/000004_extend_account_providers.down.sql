-- nexusmail:foreign_keys=off
--
-- Narrow accounts.provider back to the original four values.
--
-- Same rebuild as the up migration and the same reason for the directive: the
-- implicit DELETE FROM behind DROP TABLE would cascade into mailboxes, messages
-- and drafts.
--
-- This fails on a database that holds a 126 or icloud account, and that is the
-- honest behaviour: the four-value CHECK cannot describe those rows, and
-- silently deleting the accounts (with their mail, through the same cascade this
-- migration exists to avoid) to make the constraint fit would be worse than
-- refusing. Disconnect those accounts first if the constraint really has to be
-- narrowed.

CREATE TABLE accounts_old (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email TEXT NOT NULL COLLATE NOCASE,
    display_name TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL CHECK (provider IN ('qq', '163', 'gmail', 'outlook')),
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

INSERT INTO accounts_old (
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

ALTER TABLE accounts_old RENAME TO accounts;
