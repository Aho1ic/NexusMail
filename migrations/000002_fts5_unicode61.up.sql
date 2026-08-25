-- Switch message_fts from trigram to unicode61 with a small prefix index.
--
-- Trigram indexes every 3-gram of the indexed columns, so the index is 3-5x
-- larger than unicode61 and the messages_fts_au trigger re-tokenises the
-- full body_text on every body prefetch. The body prefetch path runs on the
-- 5-second inbox probe, so the per-update cost is on the hot path.
--
-- The trade-off is the substring search behaviour: trigram matches "Nexus"
-- inside "NexusMail" because every 3-gram window of "NexusMail" contains
-- "Nex". unicode61 tokenises "NexusMail" as one word, so a plain "Nexus"
-- query returns nothing.
--
-- The fix is `prefix='2 1'`, which adds 2- and 1-character prefix indexes on
-- top of the standard token index. The applyMessageSearch query appends `*`
-- to single-word queries, which routes them through the prefix index. CJK
-- falls back to the LIKE path (>= 3 chars hits FTS, 1-2 char CJK queries
-- fall to LIKE), so this change does not regress CJK search.

DROP TRIGGER IF EXISTS messages_fts_ai;
DROP TRIGGER IF EXISTS messages_fts_ad;
DROP TRIGGER IF EXISTS messages_fts_au;
DROP TABLE IF EXISTS message_fts;

CREATE VIRTUAL TABLE message_fts USING fts5(
    subject,
    sender,
    recipients,
    body_text,
    content='messages',
    content_rowid='id',
    tokenize="unicode61 remove_diacritics 2",
    prefix='2 1'
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
