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
	// ReadAt is when the user read it (the receipt's ts), in unix
	// milliseconds; 0 when the server did not say.
	ReadAt int64
}

// receiptColumns are Receipt's columns, in scanReceipts' order.
const receiptColumns = `room_id, thread_id, user_id, event_id, ts, read_at`

// PutReceipt records r, replacing the user's previous receipt in the same
// room and thread.
func (s *Store) PutReceipt(ctx context.Context, r Receipt) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO slkmx_receipts (`+receiptColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (room_id, thread_id, user_id) DO UPDATE SET
			event_id = excluded.event_id, ts = excluded.ts, read_at = excluded.read_at`,
		r.RoomID, r.ThreadID, r.UserID, r.EventID, r.TS, r.ReadAt)
	return err
}

// AdvanceReceipt records r unless it would move the user's receipt in
// that room and thread back, and reports whether it did. Servers may
// deliver receipts out of order (federation, one user's several
// devices), and a receipt never moves back: r replaces the stored one
// when it is on another event and its TS is later, or the stored one has
// no TS yet. A receipt without a TS (on an event not mapped yet) only
// replaces one that has none either.
func (s *Store) AdvanceReceipt(ctx context.Context, r Receipt) (bool, error) {
	res, err := s.DB.Exec(ctx, `INSERT INTO slkmx_receipts (`+receiptColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (room_id, thread_id, user_id) DO UPDATE SET
			event_id = excluded.event_id, ts = excluded.ts, read_at = excluded.read_at
		WHERE slkmx_receipts.event_id != excluded.event_id AND (
			(excluded.ts != '' AND (slkmx_receipts.ts = '' OR excluded.ts > slkmx_receipts.ts))
			OR (excluded.ts = '' AND slkmx_receipts.ts = ''))`,
		r.RoomID, r.ThreadID, r.UserID, r.EventID, r.TS, r.ReadAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// GetReceipt returns user's receipt in room and thread.
func (s *Store) GetReceipt(ctx context.Context, room id.RoomID, thread string, user id.UserID) (Receipt, bool, error) {
	r := Receipt{RoomID: room, ThreadID: thread, UserID: user}
	err := s.DB.QueryRow(ctx, `SELECT event_id, ts, read_at FROM slkmx_receipts
		WHERE room_id = $1 AND thread_id = $2 AND user_id = $3`, room, thread, user).Scan(&r.EventID, &r.TS, &r.ReadAt)
	if err != nil {
		return Receipt{}, false, ignoreNoRows(err)
	}
	return r, true, nil
}

// RoomReceipts returns every receipt in room, by user then thread.
func (s *Store) RoomReceipts(ctx context.Context, room id.RoomID) ([]Receipt, error) {
	return s.queryReceipts(ctx, `SELECT `+receiptColumns+` FROM slkmx_receipts
		WHERE room_id = $1 ORDER BY user_id, thread_id`, room)
}

// UserRoomReceipts returns user's receipts in room, by thread.
func (s *Store) UserRoomReceipts(ctx context.Context, room id.RoomID, user id.UserID) ([]Receipt, error) {
	return s.queryReceipts(ctx, `SELECT `+receiptColumns+` FROM slkmx_receipts
		WHERE room_id = $1 AND user_id = $2 ORDER BY thread_id`, room, user)
}

// queryReceipts runs a SELECT of receiptColumns.
func (s *Store) queryReceipts(ctx context.Context, query string, args ...any) ([]Receipt, error) {
	rows, err := s.DB.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Receipt
	for rows.Next() {
		var r Receipt
		if err := rows.Scan(&r.RoomID, &r.ThreadID, &r.UserID, &r.EventID, &r.TS, &r.ReadAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
