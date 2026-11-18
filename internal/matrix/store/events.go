package store

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/id"
)

// Event is a row of slkmx_events.
type Event struct {
	RoomID id.RoomID
	ID     id.EventID
	// TS is the event's slk TS, from ident; unique within the room.
	TS     string
	Sender id.UserID
	// Type is the event type, the decrypted one when Decrypted.
	Type string
	// ThreadRoot is the root of the thread the event is in, "" if none.
	ThreadRoot id.EventID
	// RelatesTo and RelType are the event's m.relates_to target and
	// rel_type (m.replace, m.annotation, m.thread, ...), "" if none.
	RelatesTo id.EventID
	RelType   string
	// Content is the event content, decrypted when Decrypted. Nil is
	// stored as {}.
	Content json.RawMessage
	// Body is the plain text the full-text index holds for the event, ""
	// for none. A redacted event's body is always "".
	Body string
	// Decrypted says the event arrived encrypted and Content and Type are
	// the decrypted ones.
	Decrypted bool
	Redacted  bool
	// OriginServerTS is the event's origin_server_ts in milliseconds.
	OriginServerTS int64
}

const eventColumns = `room_id, event_id, ts, sender, type, thread_root, relates_to_event, rel_type,
	content_json, body, decrypted, redacted, origin_server_ts`

// PutEvent inserts e or replaces every column of the stored event with the
// same room and event ID. A redacted event is stored with an empty body.
func (s *Store) PutEvent(ctx context.Context, e Event) error {
	content := string(e.Content)
	if content == "" {
		content = "{}"
	}
	if e.Redacted {
		e.Body = ""
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO slkmx_events (`+eventColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (room_id, event_id) DO UPDATE SET
			ts = excluded.ts, sender = excluded.sender, type = excluded.type,
			thread_root = excluded.thread_root, relates_to_event = excluded.relates_to_event,
			rel_type = excluded.rel_type, content_json = excluded.content_json, body = excluded.body,
			decrypted = excluded.decrypted, redacted = excluded.redacted,
			origin_server_ts = excluded.origin_server_ts`,
		e.RoomID, e.ID, e.TS, e.Sender, e.Type, e.ThreadRoot, e.RelatesTo, e.RelType,
		content, e.Body, e.Decrypted, e.Redacted, e.OriginServerTS)
	return err
}

// GetEvent returns event ev of room.
func (s *Store) GetEvent(ctx context.Context, room id.RoomID, ev id.EventID) (Event, bool, error) {
	e, err := scanEvent(s.DB.QueryRow(ctx,
		`SELECT `+eventColumns+` FROM slkmx_events WHERE room_id = $1 AND event_id = $2`, room, ev))
	return e, found(err), ignoreNoRows(err)
}

// EventsBefore returns up to limit of room's events with a TS before
// before ("" for the newest), oldest first.
func (s *Store) EventsBefore(ctx context.Context, room id.RoomID, before string, limit int) ([]Event, error) {
	if before == "" {
		// "~" sorts after every TS ("%010d.%06d").
		before = "~"
	}
	out, err := s.queryEvents(ctx, `SELECT `+eventColumns+` FROM slkmx_events
		WHERE room_id = $1 AND ts < $2 ORDER BY ts DESC LIMIT $3`, room, before, limit)
	slices.Reverse(out)
	return out, err
}

// RedactEvent marks event ev of room redacted and clears its content and
// body, which also drops it from the full-text index.
func (s *Store) RedactEvent(ctx context.Context, room id.RoomID, ev id.EventID) error {
	_, err := s.DB.Exec(ctx, `UPDATE slkmx_events SET redacted = 1, body = '', content_json = '{}'
		WHERE room_id = $1 AND event_id = $2`, room, ev)
	return err
}

// DeleteEvent removes event ev of room. Its TS mapping stays.
func (s *Store) DeleteEvent(ctx context.Context, room id.RoomID, ev id.EventID) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM slkmx_events WHERE room_id = $1 AND event_id = $2`, room, ev)
	return err
}

// SearchRoom returns up to limit of room's events whose body contains
// every word of text (each matched as a prefix, case- and
// accent-insensitively), newest first. Empty text matches nothing.
func (s *Store) SearchRoom(ctx context.Context, room id.RoomID, text string, limit int) ([]Event, error) {
	q := ftsQuery(text)
	if q == "" {
		return nil, nil
	}
	return s.queryEvents(ctx, `SELECT `+eventColumns+` FROM slkmx_events
		WHERE room_id = $1 AND seq IN (SELECT rowid FROM slkmx_events_fts WHERE slkmx_events_fts MATCH $2)
		ORDER BY ts DESC LIMIT $3`, room, q, limit)
}

// ftsQuery quotes each word of input as an FTS5 prefix term, so FTS5
// operators in it are literal text. It is the rule of
// internal/cache.buildFTSQuery, which is unexported.
func ftsQuery(input string) string {
	fields := strings.Fields(input)
	for i, f := range fields {
		fields[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"*`
	}
	return strings.Join(fields, " ")
}

func (s *Store) queryEvents(ctx context.Context, query string, args ...any) ([]Event, error) {
	rows, err := s.DB.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanEvent(row dbutil.Scannable) (Event, error) {
	var e Event
	var content string
	err := row.Scan(&e.RoomID, &e.ID, &e.TS, &e.Sender, &e.Type, &e.ThreadRoot, &e.RelatesTo, &e.RelType,
		&content, &e.Body, &e.Decrypted, &e.Redacted, &e.OriginServerTS)
	if err != nil {
		return Event{}, err
	}
	e.Content = json.RawMessage(content)
	return e, nil
}
