package store

import (
	"context"

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
	rows, err := s.DB.Query(ctx, `SELECT room_id, thread_id, event_id, ts FROM slkmx_receipts
		WHERE user_id = $1 ORDER BY room_id, thread_id`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Receipt
	for rows.Next() {
		r := Receipt{UserID: user}
		if err := rows.Scan(&r.RoomID, &r.ThreadID, &r.EventID, &r.TS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
