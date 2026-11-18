package store

import (
	"context"

	"maunium.net/go/mautrix/id"
)

// Receipt is a user's read receipt in a room or one of its threads.
type Receipt struct {
	RoomID id.RoomID
	// ThreadID is "" for an unthreaded receipt, "main" or a thread root
	// event ID for a threaded one (the receipt's thread_id).
	ThreadID string
	UserID   id.UserID
	EventID  id.EventID
	// TS is the slk TS of EventID, "" if it has none yet.
	TS string
}

// PutReceipt records r, replacing the user's previous receipt in the same
// room and thread.
func (s *Store) PutReceipt(ctx context.Context, r Receipt) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO slkmx_receipts (room_id, thread_id, user_id, event_id, ts)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (room_id, thread_id, user_id) DO UPDATE SET event_id = excluded.event_id, ts = excluded.ts`,
		r.RoomID, r.ThreadID, r.UserID, r.EventID, r.TS)
	return err
}

// GetReceipt returns user's receipt in room and thread.
func (s *Store) GetReceipt(ctx context.Context, room id.RoomID, thread string, user id.UserID) (Receipt, bool, error) {
	r := Receipt{RoomID: room, ThreadID: thread, UserID: user}
	err := s.DB.QueryRow(ctx, `SELECT event_id, ts FROM slkmx_receipts
		WHERE room_id = $1 AND thread_id = $2 AND user_id = $3`, room, thread, user).Scan(&r.EventID, &r.TS)
	if err != nil {
		return Receipt{}, false, ignoreNoRows(err)
	}
	return r, true, nil
}
