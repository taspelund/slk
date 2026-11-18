-- v1 -> v2: Room name and kind inputs
-- What the room model computes a room's name and kind from, so a restart
-- computes the same ones. inputs_saved is 0 on rows from before.
ALTER TABLE slkmx_rooms ADD COLUMN inputs_saved INTEGER NOT NULL DEFAULT 0;
ALTER TABLE slkmx_rooms ADD COLUMN has_name_event INTEGER NOT NULL DEFAULT 0;
ALTER TABLE slkmx_rooms ADD COLUMN name_event TEXT NOT NULL DEFAULT '';
ALTER TABLE slkmx_rooms ADD COLUMN heroes TEXT NOT NULL DEFAULT '[]';
ALTER TABLE slkmx_rooms ADD COLUMN have_member_counts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE slkmx_rooms ADD COLUMN joined_member_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE slkmx_rooms ADD COLUMN invited_member_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE slkmx_rooms ADD COLUMN join_rule TEXT NOT NULL DEFAULT '';
ALTER TABLE slkmx_rooms ADD COLUMN direct_users TEXT NOT NULL DEFAULT '[]';
