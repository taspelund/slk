-- v3 -> v4: Receipt times
-- read_at is when the user read up to event_id, the receipt's ts in unix
-- milliseconds (0 when the server did not say). The index finds one
-- user's receipts in a room (its threads and its main timeline).
ALTER TABLE slkmx_receipts ADD COLUMN read_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX slkmx_receipts_room_user ON slkmx_receipts (room_id, user_id);
