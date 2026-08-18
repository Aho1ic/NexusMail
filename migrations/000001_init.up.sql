CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE accounts (
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

CREATE TABLE mailboxes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL,
    remote_name TEXT NOT NULL,
    display_name TEXT NOT NULL,
    delimiter TEXT,
    role TEXT NOT NULL DEFAULT 'custom' CHECK (role IN (
        'inbox', 'sent', 'drafts', 'archive', 'trash', 'junk', 'all', 'custom'
    )),
    sync_mode TEXT NOT NULL DEFAULT 'lazy' CHECK (sync_mode IN ('realtime', 'periodic', 'lazy')),
    uid_validity INTEGER NOT NULL DEFAULT 0,
    uid_next INTEGER,
    highest_modseq INTEGER,
    last_uid INTEGER NOT NULL DEFAULT 0,
    last_sync_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE,
    UNIQUE (account_id, remote_name)
);

CREATE TABLE blob_objects (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    storage_key TEXT NOT NULL UNIQUE,
    sha256 BLOB NOT NULL UNIQUE,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    durability TEXT NOT NULL CHECK (durability IN ('cache', 'durable')),
    last_accessed_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL,
    direction TEXT NOT NULL DEFAULT 'incoming' CHECK (direction IN ('incoming', 'outgoing')),
    dedupe_key BLOB NOT NULL,
    rfc_message_id TEXT,
    provider_message_id TEXT,
    in_reply_to TEXT,
    references_json TEXT NOT NULL DEFAULT '[]',
    subject TEXT NOT NULL DEFAULT '',
    sender TEXT NOT NULL DEFAULT '',
    recipients TEXT NOT NULL DEFAULT '',
    from_json TEXT NOT NULL DEFAULT '[]',
    to_json TEXT NOT NULL DEFAULT '[]',
    cc_json TEXT NOT NULL DEFAULT '[]',
    bcc_json TEXT NOT NULL DEFAULT '[]',
    reply_to_json TEXT NOT NULL DEFAULT '[]',
    snippet TEXT NOT NULL DEFAULT '',
    body_text TEXT NOT NULL DEFAULT '',
    body_html TEXT NOT NULL DEFAULT '',
    body_state TEXT NOT NULL DEFAULT 'metadata' CHECK (body_state IN (
        'metadata', 'queued', 'fetching', 'ready', 'error'
    )),
    raw_blob_id INTEGER,
    size_bytes INTEGER NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    sent_at INTEGER,
    received_at INTEGER NOT NULL,
    is_read INTEGER NOT NULL DEFAULT 0 CHECK (is_read IN (0, 1)),
    is_starred INTEGER NOT NULL DEFAULT 0 CHECK (is_starred IN (0, 1)),
    has_attachments INTEGER NOT NULL DEFAULT 0 CHECK (has_attachments IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE,
    FOREIGN KEY (raw_blob_id) REFERENCES blob_objects(id) ON DELETE SET NULL,
    UNIQUE (account_id, dedupe_key)
);

CREATE TABLE mailbox_messages (
    mailbox_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    uid INTEGER NOT NULL CHECK (uid > 0),
    flags_json TEXT NOT NULL DEFAULT '[]',
    internal_date INTEGER NOT NULL,
    PRIMARY KEY (mailbox_id, uid),
    FOREIGN KEY (mailbox_id) REFERENCES mailboxes(id) ON DELETE CASCADE,
    FOREIGN KEY (message_id) REFERENCES messages(id) ON DELETE CASCADE
);

CREATE TABLE attachments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id INTEGER NOT NULL,
    part_id TEXT NOT NULL,
    filename TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    disposition TEXT NOT NULL DEFAULT 'attachment' CHECK (disposition IN ('attachment', 'inline')),
    content_id TEXT,
    size_bytes INTEGER NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    fetch_state TEXT NOT NULL DEFAULT 'metadata' CHECK (fetch_state IN (
        'metadata', 'fetching', 'ready', 'error'
    )),
    blob_id INTEGER,
    last_error TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (message_id) REFERENCES messages(id) ON DELETE CASCADE,
    FOREIGN KEY (blob_id) REFERENCES blob_objects(id) ON DELETE SET NULL,
    UNIQUE (message_id, part_id)
);

CREATE TABLE drafts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL,
    source_message_id INTEGER,
    conflict_of_id INTEGER,
    sent_message_id INTEGER,
    rfc_message_id TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1,
    to_json TEXT NOT NULL DEFAULT '[]',
    cc_json TEXT NOT NULL DEFAULT '[]',
    bcc_json TEXT NOT NULL DEFAULT '[]',
    subject TEXT NOT NULL DEFAULT '',
    body_text TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN (
        'draft', 'queued', 'sending', 'retry_wait', 'failed', 'unknown', 'sent'
    )),
    remote_sync_state TEXT NOT NULL DEFAULT 'dirty' CHECK (remote_sync_state IN (
        'dirty', 'syncing', 'synced', 'error', 'conflict'
    )),
    remote_mailbox_id INTEGER,
    remote_uid_validity INTEGER,
    remote_uid INTEGER,
    remote_updated_at INTEGER,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER,
    last_smtp_code INTEGER,
    last_error TEXT,
    sent_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE,
    FOREIGN KEY (source_message_id) REFERENCES messages(id) ON DELETE SET NULL,
    FOREIGN KEY (conflict_of_id) REFERENCES drafts(id) ON DELETE SET NULL,
    FOREIGN KEY (sent_message_id) REFERENCES messages(id) ON DELETE SET NULL,
    FOREIGN KEY (remote_mailbox_id) REFERENCES mailboxes(id) ON DELETE SET NULL,
    UNIQUE (account_id, rfc_message_id)
);

CREATE TABLE draft_attachments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    draft_id INTEGER NOT NULL,
    blob_id INTEGER NOT NULL,
    filename TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    position INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (draft_id) REFERENCES drafts(id) ON DELETE CASCADE,
    FOREIGN KEY (blob_id) REFERENCES blob_objects(id) ON DELETE RESTRICT
);

CREATE TABLE web_sessions (
    token_hash BLOB PRIMARY KEY,
    csrf_hash BLOB NOT NULL,
    expires_at INTEGER NOT NULL,
    absolute_expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL
);

CREATE INDEX idx_mailboxes_account_role ON mailboxes(account_id, role);
CREATE INDEX idx_messages_feed ON messages(received_at DESC, id DESC);
CREATE INDEX idx_messages_account_feed ON messages(account_id, received_at DESC, id DESC);
CREATE INDEX idx_messages_account_read_feed ON messages(account_id, is_read, received_at DESC, id DESC);
CREATE INDEX idx_messages_rfc_id ON messages(account_id, rfc_message_id);
CREATE INDEX idx_mailbox_messages_message ON mailbox_messages(message_id);
CREATE INDEX idx_drafts_status_schedule ON drafts(status, next_attempt_at);
CREATE UNIQUE INDEX idx_drafts_remote_uid ON drafts(remote_mailbox_id, remote_uid)
    WHERE remote_mailbox_id IS NOT NULL AND remote_uid IS NOT NULL;
CREATE INDEX idx_blob_lru ON blob_objects(durability, last_accessed_at);
CREATE INDEX idx_sessions_expiry ON web_sessions(expires_at);

CREATE VIRTUAL TABLE message_fts USING fts5(
    subject,
    sender,
    recipients,
    body_text,
    content = 'messages',
    content_rowid = 'id',
    tokenize = 'trigram case_sensitive 0'
);

CREATE TRIGGER messages_fts_ai AFTER INSERT ON messages BEGIN
    INSERT INTO message_fts(rowid, subject, sender, recipients, body_text)
    VALUES (new.id, new.subject, new.sender, new.recipients, new.body_text);
END;

CREATE TRIGGER messages_fts_ad AFTER DELETE ON messages BEGIN
    INSERT INTO message_fts(message_fts, rowid, subject, sender, recipients, body_text)
    VALUES ('delete', old.id, old.subject, old.sender, old.recipients, old.body_text);
END;

CREATE TRIGGER messages_fts_au AFTER UPDATE OF subject, sender, recipients, body_text ON messages BEGIN
    INSERT INTO message_fts(message_fts, rowid, subject, sender, recipients, body_text)
    VALUES ('delete', old.id, old.subject, old.sender, old.recipients, old.body_text);
    INSERT INTO message_fts(rowid, subject, sender, recipients, body_text)
    VALUES (new.id, new.subject, new.sender, new.recipients, new.body_text);
END;

