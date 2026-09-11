-- Rebuild the message_fts index from the messages table.
--
-- Migration 000002 dropped and recreated message_fts to switch the tokenizer,
-- but an external-content FTS5 table holds no data of its own at creation: the
-- new index starts empty and only the triggers populate it, and they only fire
-- on writes that happen afterwards. Every message that existed before 000002
-- ran is therefore absent from the index, and the >= 3-character branch of
-- applyMessageSearch (store.go) joins message_fts and returns zero hits for
-- them instead of degrading to the LIKE path. Search silently lost the entire
-- pre-existing backlog while still working for newly synced mail.
--
-- The breakage is invisible to a naive row count: `count(*)` on an
-- external-content table is answered from the content table (messages), so it
-- reports the full message count no matter how little is actually indexed.
--
-- 'rebuild' discards the index and re-tokenises every content row, which is why
-- it repairs the gap without touching the schema 000002 established.

INSERT INTO message_fts(message_fts) VALUES('rebuild');
