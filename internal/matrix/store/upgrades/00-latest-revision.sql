-- v0 -> v3: Latest revision
-- The /sync position. One row (id 0), created here so reads never miss.
CREATE TABLE slkmx_sync_state (
	id         INTEGER PRIMARY KEY CHECK (id = 0),
	next_batch TEXT    NOT NULL DEFAULT '',
	filter_id  TEXT    NOT NULL DEFAULT ''
);
INSERT INTO slkmx_sync_state (id) VALUES (0);

-- Rooms the account knows about. last_event_ts is the slk TS of the newest
-- event ("%010d.%06d"), which sorts across rooms too. The columns from
-- inputs_saved on are what the room model computes name and kind from
-- (m.room.name, the sync summary, the join rule, the m.direct users; the
-- lists as JSON arrays); inputs_saved is 0 on rows written before they
-- existed (v1).
CREATE TABLE slkmx_rooms (
	room_id            TEXT    PRIMARY KEY,
	canonical_alias    TEXT    NOT NULL DEFAULT '',
	name               TEXT    NOT NULL DEFAULT '',
	topic              TEXT    NOT NULL DEFAULT '',
	kind               TEXT    NOT NULL CHECK (kind IN ('channel', 'private', 'dm', 'group_dm')),
	is_space           INTEGER NOT NULL DEFAULT 0,
	encrypted          INTEGER NOT NULL DEFAULT 0,
	join_state         TEXT    NOT NULL CHECK (join_state IN ('join', 'invite', 'leave')),
	dm_user_id         TEXT    NOT NULL DEFAULT '',
	avatar_mxc         TEXT    NOT NULL DEFAULT '',
	last_event_ts      TEXT    NOT NULL DEFAULT '',
	highlight_count    INTEGER NOT NULL DEFAULT 0,
	notification_count INTEGER NOT NULL DEFAULT 0,
	marked_unread      INTEGER NOT NULL DEFAULT 0,
	updated_at         INTEGER NOT NULL DEFAULT 0,
	inputs_saved         INTEGER NOT NULL DEFAULT 0,
	has_name_event       INTEGER NOT NULL DEFAULT 0,
	name_event           TEXT    NOT NULL DEFAULT '',
	heroes               TEXT    NOT NULL DEFAULT '[]',
	have_member_counts   INTEGER NOT NULL DEFAULT 0,
	joined_member_count  INTEGER NOT NULL DEFAULT 0,
	invited_member_count INTEGER NOT NULL DEFAULT 0,
	join_rule            TEXT    NOT NULL DEFAULT '',
	direct_users         TEXT    NOT NULL DEFAULT '[]',
	inviter              TEXT    NOT NULL DEFAULT '',
	alt_aliases          TEXT    NOT NULL DEFAULT '[]'
);

-- m.space.child edges: room_id is in space space_id, ordered by
-- child_order (the child event's "order", '' when unset).
CREATE TABLE slkmx_room_space_parents (
	room_id     TEXT NOT NULL,
	space_id    TEXT NOT NULL,
	child_order TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (room_id, space_id)
) WITHOUT ROWID;
CREATE INDEX slkmx_room_space_parents_space_idx ON slkmx_room_space_parents (space_id, child_order);

-- Timeline events. seq is an explicit INTEGER PRIMARY KEY so the full-text
-- index's rowids survive VACUUM. content_json is the (decrypted, when
-- decrypted = 1) content; body is the plain text the full-text index holds.
CREATE TABLE slkmx_events (
	seq              INTEGER PRIMARY KEY,
	room_id          TEXT    NOT NULL,
	event_id         TEXT    NOT NULL,
	ts               TEXT    NOT NULL,
	sender           TEXT    NOT NULL,
	type             TEXT    NOT NULL,
	thread_root      TEXT    NOT NULL DEFAULT '',
	relates_to_event TEXT    NOT NULL DEFAULT '',
	rel_type         TEXT    NOT NULL DEFAULT '',
	content_json     TEXT    NOT NULL DEFAULT '{}',
	body             TEXT    NOT NULL DEFAULT '',
	decrypted        INTEGER NOT NULL DEFAULT 0,
	redacted         INTEGER NOT NULL DEFAULT 0,
	origin_server_ts INTEGER NOT NULL DEFAULT 0,
	UNIQUE (room_id, event_id),
	CHECK (redacted = 0 OR body = '')
);
CREATE UNIQUE INDEX slkmx_events_room_ts_idx ON slkmx_events (room_id, ts);
CREATE INDEX slkmx_events_thread_idx ON slkmx_events (room_id, thread_root, ts);
CREATE INDEX slkmx_events_relates_idx ON slkmx_events (room_id, relates_to_event);

-- ident's TS <-> event ID map.
CREATE TABLE slkmx_ts_map (
	room_id  TEXT NOT NULL,
	event_id TEXT NOT NULL,
	ts       TEXT NOT NULL,
	PRIMARY KEY (room_id, event_id),
	UNIQUE (room_id, ts)
) WITHOUT ROWID;

-- ident's alias <-> Matrix ID map.
CREATE TABLE slkmx_aliases (
	alias     TEXT PRIMARY KEY,
	kind      TEXT NOT NULL CHECK (kind IN ('user', 'room')),
	matrix_id TEXT NOT NULL,
	UNIQUE (kind, matrix_id)
) WITHOUT ROWID;

-- Read receipts. thread_id is '' for an unthreaded receipt, 'main' or a
-- thread root event ID for a threaded one; ts is the slk TS of event_id
-- ('' when it has none yet).
CREATE TABLE slkmx_receipts (
	room_id   TEXT NOT NULL,
	thread_id TEXT NOT NULL,
	user_id   TEXT NOT NULL,
	event_id  TEXT NOT NULL,
	ts        TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (room_id, thread_id, user_id)
) WITHOUT ROWID;

-- Holes in a room's stored timeline (a limited /sync): paginate back from
-- prev_batch to fill the events before after_ts.
CREATE TABLE slkmx_gaps (
	gap_id     INTEGER PRIMARY KEY,
	room_id    TEXT    NOT NULL,
	prev_batch TEXT    NOT NULL,
	after_ts   TEXT    NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX slkmx_gaps_room_idx ON slkmx_gaps (room_id, after_ts);

-- Downloaded media on disk, for eviction by last_used.
CREATE TABLE slkmx_media_cache_index (
	mxc       TEXT    PRIMARY KEY,
	path      TEXT    NOT NULL,
	size      INTEGER NOT NULL,
	last_used INTEGER NOT NULL
);
CREATE INDEX slkmx_media_cache_index_last_used_idx ON slkmx_media_cache_index (last_used);

-- Full-text index over slkmx_events.body (rowid = seq), kept in step by
-- the triggers below. A redacted event has an empty body (the CHECK on
-- slkmx_events), so it has no terms in the index.
CREATE VIRTUAL TABLE slkmx_events_fts USING fts5 (
	body,
	content = 'slkmx_events',
	content_rowid = 'seq',
	tokenize = 'unicode61 remove_diacritics 2'
);
CREATE TRIGGER slkmx_events_fts_ai AFTER INSERT ON slkmx_events BEGIN
	INSERT INTO slkmx_events_fts (rowid, body) VALUES (new.seq, new.body);
END;
CREATE TRIGGER slkmx_events_fts_ad AFTER DELETE ON slkmx_events BEGIN
	INSERT INTO slkmx_events_fts (slkmx_events_fts, rowid, body) VALUES ('delete', old.seq, old.body);
END;
CREATE TRIGGER slkmx_events_fts_au AFTER UPDATE OF body ON slkmx_events BEGIN
	INSERT INTO slkmx_events_fts (slkmx_events_fts, rowid, body) VALUES ('delete', old.seq, old.body);
	INSERT INTO slkmx_events_fts (rowid, body) VALUES (new.seq, new.body);
END;
