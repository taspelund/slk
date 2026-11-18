package store

import (
	"context"
	"encoding/json"
	"fmt"
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

	// The inputs the room model computes Name, Kind and DMUserID from,
	// so a restart computes the same. InputsSaved is false on rows
	// written before these were stored (schema v1), whose other input
	// fields are zero.
	InputsSaved bool
	// HasNameEvent says an m.room.name was seen; NameEvent is its name.
	HasNameEvent bool
	NameEvent    string
	// Heroes and the member counts are the sync summary's;
	// HaveMemberCounts says the counts are known.
	Heroes             []id.UserID
	HaveMemberCounts   bool
	JoinedMemberCount  int
	InvitedMemberCount int
	// JoinRule is the m.room.join_rules join rule, "" if none was seen.
	JoinRule string
	// DirectUsers are the users m.direct lists the room under, sorted;
	// empty when the room is not in m.direct.
	DirectUsers []id.UserID
}

const roomColumns = `room_id, canonical_alias, name, topic, kind, is_space, encrypted, join_state,
	dm_user_id, avatar_mxc, last_event_ts, highlight_count, notification_count, marked_unread, updated_at,
	inputs_saved, has_name_event, name_event, heroes, have_member_counts, joined_member_count,
	invited_member_count, join_rule, direct_users`

// PutRoom inserts r or replaces every column of the stored row, and sets
// its UpdatedAt to now.
func (s *Store) PutRoom(ctx context.Context, r Room) error {
	heroes, err := userList(r.Heroes)
	if err != nil {
		return err
	}
	direct, err := userList(r.DirectUsers)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO slkmx_rooms (`+roomColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
			$16, $17, $18, $19, $20, $21, $22, $23, $24)
		ON CONFLICT (room_id) DO UPDATE SET
			canonical_alias = excluded.canonical_alias, name = excluded.name, topic = excluded.topic,
			kind = excluded.kind, is_space = excluded.is_space, encrypted = excluded.encrypted,
			join_state = excluded.join_state, dm_user_id = excluded.dm_user_id,
			avatar_mxc = excluded.avatar_mxc, last_event_ts = excluded.last_event_ts,
			highlight_count = excluded.highlight_count, notification_count = excluded.notification_count,
			marked_unread = excluded.marked_unread, updated_at = excluded.updated_at,
			inputs_saved = excluded.inputs_saved, has_name_event = excluded.has_name_event,
			name_event = excluded.name_event, heroes = excluded.heroes,
			have_member_counts = excluded.have_member_counts,
			joined_member_count = excluded.joined_member_count,
			invited_member_count = excluded.invited_member_count,
			join_rule = excluded.join_rule, direct_users = excluded.direct_users`,
		r.ID, r.CanonicalAlias, r.Name, r.Topic, r.Kind, r.IsSpace, r.Encrypted, r.JoinState,
		r.DMUserID, r.AvatarMXC, r.LastEventTS, r.HighlightCount, r.NotificationCount, r.MarkedUnread,
		time.Now().UnixMilli(),
		r.InputsSaved, r.HasNameEvent, r.NameEvent, heroes, r.HaveMemberCounts, r.JoinedMemberCount,
		r.InvitedMemberCount, r.JoinRule, direct)
	return err
}

// userList is how a list of users is stored: a JSON array.
func userList(users []id.UserID) (string, error) {
	if len(users) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(users)
	return string(b), err
}

// parseUserList reads a userList value; an empty list is nil.
func parseUserList(s string) ([]id.UserID, error) {
	var out []id.UserID
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
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
	var heroes, direct string
	err := row.Scan(&r.ID, &r.CanonicalAlias, &r.Name, &r.Topic, &r.Kind, &r.IsSpace, &r.Encrypted,
		&r.JoinState, &r.DMUserID, &r.AvatarMXC, &r.LastEventTS, &r.HighlightCount, &r.NotificationCount,
		&r.MarkedUnread, &updated,
		&r.InputsSaved, &r.HasNameEvent, &r.NameEvent, &heroes, &r.HaveMemberCounts, &r.JoinedMemberCount,
		&r.InvitedMemberCount, &r.JoinRule, &direct)
	if err != nil {
		return Room{}, err
	}
	r.UpdatedAt = time.UnixMilli(updated)
	if r.Heroes, err = parseUserList(heroes); err != nil {
		return Room{}, fmt.Errorf("store: heroes of %s: %w", r.ID, err)
	}
	if r.DirectUsers, err = parseUserList(direct); err != nil {
		return Room{}, fmt.Errorf("store: direct users of %s: %w", r.ID, err)
	}
	return r, nil
}
