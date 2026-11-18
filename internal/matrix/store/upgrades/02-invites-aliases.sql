-- v2 -> v3: Invite senders and alternative aliases
-- inviter is who sent a pending invite; alt_aliases are the
-- m.room.canonical_alias event's alt_aliases, as a JSON array.
ALTER TABLE slkmx_rooms ADD COLUMN inviter TEXT NOT NULL DEFAULT '';
ALTER TABLE slkmx_rooms ADD COLUMN alt_aliases TEXT NOT NULL DEFAULT '[]';
