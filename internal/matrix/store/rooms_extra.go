package store

import (
	"context"
	"database/sql"
	"time"

	"maunium.net/go/mautrix/id"
)

// SpaceParent is a row of slkmx_room_space_parents: room is in space
// SpaceID, ordered by Order (the m.space.child event's "order", "" when
// unset or when the edge comes from the room's m.space.parent).
type SpaceParent struct {
	RoomID  id.RoomID
	SpaceID id.RoomID
	Order   string
}

// PutSpaceParent records p, replacing the order of an existing edge.
func (s *Store) PutSpaceParent(ctx context.Context, p SpaceParent) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO slkmx_room_space_parents (room_id, space_id, child_order)
		VALUES ($1, $2, $3) ON CONFLICT (room_id, space_id) DO UPDATE SET child_order = excluded.child_order`,
		p.RoomID, p.SpaceID, p.Order)
	return err
}

// DeleteSpaceParent removes the edge putting room in space.
func (s *Store) DeleteSpaceParent(ctx context.Context, room, space id.RoomID) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM slkmx_room_space_parents WHERE room_id = $1 AND space_id = $2`,
		room, space)
	return err
}

// SpaceParents returns every space edge, by room then space.
func (s *Store) SpaceParents(ctx context.Context) ([]SpaceParent, error) {
	rows, err := s.DB.Query(ctx, `SELECT room_id, space_id, child_order FROM slkmx_room_space_parents
		ORDER BY room_id, space_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpaceParent
	for rows.Next() {
		var p SpaceParent
		if err := rows.Scan(&p.RoomID, &p.SpaceID, &p.Order); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UserReceipts returns every receipt of user, by room then thread.
func (s *Store) UserReceipts(ctx context.Context, user id.UserID) ([]Receipt, error) {
	return s.queryReceipts(ctx, `SELECT `+receiptColumns+` FROM slkmx_receipts
		WHERE user_id = $1 ORDER BY room_id, thread_id`, user)
}

// unreadTypes are the event types that can make a room unread: messages,
// stickers, and messages still encrypted (most encrypted events are).
const unreadTypes = `('m.room.message', 'm.sticker', 'm.room.encrypted')`

// LatestUnreadCandidate returns the TS of room's newest main-timeline event
// that would make the room unread for self: a message, sticker or still
// encrypted event, not sent by self, not a notice, not redacted, not in a
// thread, and not an edit or reaction. It returns "" when there is none.
func (s *Store) LatestUnreadCandidate(ctx context.Context, room id.RoomID, self id.UserID) (string, error) {
	var ts string
	err := s.DB.QueryRow(ctx, `SELECT coalesce(max(ts), '') FROM slkmx_events
		WHERE room_id = $1 AND sender <> $2 AND type IN `+unreadTypes+`
			AND thread_root = '' AND rel_type NOT IN ('m.replace', 'm.annotation') AND redacted = 0
			AND NOT (type = 'm.room.message' AND coalesce(json_extract(content_json, $3), '') = 'm.notice')`,
		room, self, "$.msgtype").Scan(&ts)
	return ts, err
}

// LatestSentBy returns the TS of the newest main-timeline event user sent
// in room, "" when there is none.
func (s *Store) LatestSentBy(ctx context.Context, room id.RoomID, user id.UserID) (string, error) {
	var ts string
	err := s.DB.QueryRow(ctx, `SELECT coalesce(max(ts), '') FROM slkmx_events
		WHERE room_id = $1 AND sender = $2 AND thread_root = ''`, room, user).Scan(&ts)
	return ts, err
}

// Relations returns the events of room that relate to target (edits,
// reactions, thread replies, ...), oldest TS first. relType filters by
// rel_type when it is not "".
func (s *Store) Relations(ctx context.Context, room id.RoomID, target id.EventID, relType string) ([]Event, error) {
	return s.queryEvents(ctx, `SELECT `+eventColumns+` FROM slkmx_events
		WHERE room_id = $1 AND relates_to_event = $2 AND ($3 = '' OR rel_type = $3) ORDER BY ts`,
		room, target, relType)
}

// Gap returns the gap with ID gapID.
func (s *Store) Gap(ctx context.Context, gapID int64) (Gap, bool, error) {
	g := Gap{ID: gapID}
	var created int64
	err := s.DB.QueryRow(ctx, `SELECT room_id, prev_batch, after_ts, created_at FROM slkmx_gaps
		WHERE gap_id = $1`, gapID).Scan(&g.RoomID, &g.PrevBatch, &g.AfterTS, &created)
	if err != nil {
		return Gap{}, false, ignoreNoRows(err)
	}
	g.CreatedAt = time.UnixMilli(created)
	return g, true, nil
}

// UpdateGap moves gap gapID: the events before afterTS are missing back
// to wherever paginating from prevBatch reaches.
func (s *Store) UpdateGap(ctx context.Context, gapID int64, prevBatch, afterTS string) error {
	_, err := s.DB.Exec(ctx, `UPDATE slkmx_gaps SET prev_batch = $1, after_ts = $2 WHERE gap_id = $3`,
		prevBatch, afterTS, gapID)
	return err
}

// TSBefore returns the newest TS mapped in room (ident's map, which
// includes events not stored as rows) that sorts before ts.
func (s *Store) TSBefore(ctx context.Context, room id.RoomID, ts string) (string, bool, error) {
	var out sql.NullString
	err := s.DB.QueryRow(ctx, `SELECT max(ts) FROM slkmx_ts_map WHERE room_id = $1 AND ts < $2`,
		room, ts).Scan(&out)
	if err != nil || !out.Valid {
		return "", false, err
	}
	return out.String, true, nil
}
