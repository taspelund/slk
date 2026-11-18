package store

import (
	"context"
	"time"

	"maunium.net/go/mautrix/id"
)

// Gap is a hole in a room's stored timeline: the events before AfterTS
// are missing back to wherever paginating from PrevBatch reaches.
type Gap struct {
	ID        int64
	RoomID    id.RoomID
	PrevBatch string
	AfterTS   string
	CreatedAt time.Time
}

// AddGap records a gap in room and returns its ID.
func (s *Store) AddGap(ctx context.Context, room id.RoomID, prevBatch, afterTS string) (int64, error) {
	var gapID int64
	err := s.DB.QueryRow(ctx, `INSERT INTO slkmx_gaps (room_id, prev_batch, after_ts, created_at)
		VALUES ($1, $2, $3, $4) RETURNING gap_id`,
		room, prevBatch, afterTS, time.Now().UnixMilli()).Scan(&gapID)
	return gapID, err
}

// Gaps returns room's gaps, oldest AfterTS first.
func (s *Store) Gaps(ctx context.Context, room id.RoomID) ([]Gap, error) {
	rows, err := s.DB.Query(ctx, `SELECT gap_id, prev_batch, after_ts, created_at FROM slkmx_gaps
		WHERE room_id = $1 ORDER BY after_ts, gap_id`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Gap
	for rows.Next() {
		g := Gap{RoomID: room}
		var created int64
		if err := rows.Scan(&g.ID, &g.PrevBatch, &g.AfterTS, &created); err != nil {
			return nil, err
		}
		g.CreatedAt = time.UnixMilli(created)
		out = append(out, g)
	}
	return out, rows.Err()
}

// RemoveGap deletes the gap with ID gapID.
func (s *Store) RemoveGap(ctx context.Context, gapID int64) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM slkmx_gaps WHERE gap_id = $1`, gapID)
	return err
}
