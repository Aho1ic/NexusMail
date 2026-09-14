-- OAuth client credentials configurable at runtime.
--
-- The Client ID and Secret used to come only from the environment, which meant a
-- deployment that had not set NEXUSMAIL_MICROSOFT_CLIENT_ID could not connect an
-- Outlook mailbox at all: the authorize button answered "missing Microsoft OAuth
-- client credentials" and the only way out was editing .env and restarting the
-- container. Storing them here lets the settings page configure the same pair
-- without a restart; the environment stays the boot default and this table wins
-- when a row exists.
--
-- One row per provider, so the provider name is the primary key rather than a
-- surrogate id: there is nothing to reference this table and an upsert on the
-- natural key is what the settings page performs.
--
-- client_secret_ciphertext carries the same sealed envelope accounts.secret_ciphertext
-- uses (AES-256-GCM under the master key). client_id is not a secret — it travels
-- in every authorization URL — and is stored in the clear so the settings page can
-- show which client is configured without opening the envelope.
--
-- The CHECK mirrors the OAuth subset of accounts.provider. The password providers
-- are deliberately absent: a row for them would be dead configuration that no code
-- path reads.
CREATE TABLE oauth_clients (
    provider TEXT PRIMARY KEY CHECK (provider IN ('gmail', 'outlook')),
    client_id TEXT NOT NULL,
    client_secret_ciphertext BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
