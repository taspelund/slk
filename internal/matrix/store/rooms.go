package store

import (
	"context"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/id"
)

// RoomKind is what kind of Slack conversation a room shows as.
type RoomKind string

const (
	RoomChannel RoomKind = "channel"
	RoomPrivate RoomKind = "private"
	RoomDM      RoomKind = "dm"
	RoomGroupDM RoomKind = "group_dm"
)

// JoinState is the account's membership in a room.
type JoinState string

const (
	Joined  JoinState = "join"
	Invited JoinState = "invite"
	Left    JoinState = "leave"
)

// Room is a row of slkmx_rooms.
type Room struct {
	ID             id.RoomID
	CanonicalAlias id.RoomAlias
	Name           string
	Topic          string
	Kind           RoomKind
	IsSpace        bool
	Encrypted      bool
	JoinState      JoinState
	// DMUserID is the other user of a DM, "" otherwise.
	DMUserID  id.UserID
	AvatarMXC id.ContentURIString
	// LastEventTS is the slk TS of the room's newest event, "" if none;
	// rooms sort by it.
	LastEventTS       string
	HighlightCount    int
	NotificationCount int
	MarkedUnread      bool
	// UpdatedAt is set by PutRoom.
	UpdatedAt time.Time
}

const roomColumns = `room_id, canonical_alias, name, topic, kind, is_space, encrypted, join_state,
	dm_user_id, avatar_mxc, last_event_ts, highlight_count, notification_count, marked_unread, updated_at`

// PutRoom inserts r or replaces every column of the stored row, and sets
// its UpdatedAt to now.
func (s *Store) PutRoom(ctx context.Context, r Room) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO slkmx_rooms (`+roomColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (room_id) DO UPDATE SET
			canonical_alias = excluded.canonical_alias, name = excluded.name, topic = excluded.topic,
			kind = excluded.kind, is_space = excluded.is_space, encrypted = excluded.encrypted,
			join_state = excluded.join_state, dm_user_id = excluded.dm_user_id,
			avatar_mxc = excluded.avatar_mxc, last_event_ts = excluded.last_event_ts,
			highlight_count = excluded.highlight_count, notification_count = excluded.notification_count,
			marked_unread = excluded.marked_unread, updated_at = excluded.updated_at`,
		r.ID, r.CanonicalAlias, r.Name, r.Topic, r.Kind, r.IsSpace, r.Encrypted, r.JoinState,
		r.DMUserID, r.AvatarMXC, r.LastEventTS, r.HighlightCount, r.NotificationCount, r.MarkedUnread,
		time.Now().UnixMilli())
	return err
}

// GetRoom returns the room with ID room.
func (s *Store) GetRoom(ctx context.Context, room id.RoomID) (Room, bool, error) {
	r, err := scanRoom(s.DB.QueryRow(ctx, `SELECT `+roomColumns+` FROM slkmx_rooms WHERE room_id = $1`, room))
	return r, found(err), ignoreNoRows(err)
}

// ListRooms returns every room, newest LastEventTS first.
func (s *Store) ListRooms(ctx context.Context) ([]Room, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+roomColumns+` FROM slkmx_rooms ORDER BY last_event_ts DESC, room_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Room
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanRoom(row dbutil.Scannable) (Room, error) {
	var r Room
	var updated int64
	err := row.Scan(&r.ID, &r.CanonicalAlias, &r.Name, &r.Topic, &r.Kind, &r.IsSpace, &r.Encrypted,
		&r.JoinState, &r.DMUserID, &r.AvatarMXC, &r.LastEventTS, &r.HighlightCount, &r.NotificationCount,
		&r.MarkedUnread, &updated)
	if err != nil {
		return Room{}, err
	}
	r.UpdatedAt = time.UnixMilli(updated)
	return r, nil
}
