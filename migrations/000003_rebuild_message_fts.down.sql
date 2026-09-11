-- Reverting this migration also rebuilds.
--
-- The FTS index is derived state, not schema: there is nothing to undo, and
-- restoring the "before" state would mean deliberately reintroducing the empty
-- index that made search miss the whole pre-000002 backlog. A rebuild is the
-- only honest inverse — it leaves the index consistent with the messages table
-- in both directions.

INSERT INTO message_fts(message_fts) VALUES('rebuild');
