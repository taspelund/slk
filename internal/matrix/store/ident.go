package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/matrix/ident"
)

var _ ident.Persistence = (*Store)(nil)

// LookupTS returns the TS mapped to ev in room.
func (s *Store) LookupTS(ctx context.Context, room id.RoomID, ev id.EventID) (string, bool, error) {
	var ts string
	err := s.DB.QueryRow(ctx,
		`SELECT ts FROM slkmx_ts_map WHERE room_id = $1 AND event_id = $2`, room, ev).Scan(&ts)
	return ts, found(err), ignoreNoRows(err)
}

// LookupEvent returns the event mapped to ts in room.
func (s *Store) LookupEvent(ctx context.Context, room id.RoomID, ts string) (id.EventID, bool, error) {
	var ev id.EventID
	err := s.DB.QueryRow(ctx,
		`SELECT event_id FROM slkmx_ts_map WHERE room_id = $1 AND ts = $2`, room, ts).Scan(&ev)
	return ev, found(err), ignoreNoRows(err)
}

// RoomBounds returns the oldest and newest TS mapped in room.
func (s *Store) RoomBounds(ctx context.Context, room id.RoomID) (oldest, newest string, ok bool, err error) {
	var lo, hi sql.NullString
	err = s.DB.QueryRow(ctx,
		`SELECT min(ts), max(ts) FROM slkmx_ts_map WHERE room_id = $1`, room).Scan(&lo, &hi)
	if err != nil || !lo.Valid {
		return "", "", false, err
	}
	return lo.String, hi.String, true, nil
}

// PutTS records ev <-> ts in room. It returns ident.ErrTSTaken if ts is
// mapped to a different event in room; mapping the same pair again is not
// an error. Mapping ev to a TS other than the one it already has is an
// error too (not ErrTSTaken): an event's TS never changes.
func (s *Store) PutTS(ctx context.Context, room id.RoomID, ev id.EventID, ts string) error {
	res, err := s.DB.Exec(ctx,
		`INSERT INTO slkmx_ts_map (room_id, event_id, ts) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		room, ev, ts)
	if err != nil {
		return fmt.Errorf("store: putting TS %s for %s in %s: %w", ts, ev, room, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 1 {
		return nil
	}
	// A row conflicted. Rows are never changed or removed, so reading
	// it back outside a transaction is safe.
	cur, ok, err := s.LookupTS(ctx, room, ev)
	switch {
	case err != nil:
		return err
	case !ok:
		return ident.ErrTSTaken
	case cur == ts:
		return nil
	default:
		return fmt.Errorf("store: event %s in %s already has TS %s, not %s: %w", ev, room, cur, ts, ident.ErrAlreadyMapped)
	}
}

// kindName is how an ident.Kind is stored.
func kindName(k ident.Kind) (string, error) {
	switch k {
	case ident.KindUser:
		return "user", nil
	case ident.KindRoom:
		return "room", nil
	}
	return "", fmt.Errorf("store: unknown alias kind %d", k)
}

// LookupAlias returns the alias of a Matrix ID.
func (s *Store) LookupAlias(ctx context.Context, kind ident.Kind, matrixID string) (string, bool, error) {
	k, err := kindName(kind)
	if err != nil {
		return "", false, err
	}
	var alias string
	err = s.DB.QueryRow(ctx,
		`SELECT alias FROM slkmx_aliases WHERE kind = $1 AND matrix_id = $2`, k, matrixID).Scan(&alias)
	return alias, found(err), ignoreNoRows(err)
}

// LookupMatrixID returns the Matrix ID an alias names.
func (s *Store) LookupMatrixID(ctx context.Context, alias string) (string, ident.Kind, bool, error) {
	var matrixID, k string
	err := s.DB.QueryRow(ctx,
		`SELECT matrix_id, kind FROM slkmx_aliases WHERE alias = $1`, alias).Scan(&matrixID, &k)
	if err != nil {
		return "", 0, false, ignoreNoRows(err)
	}
	switch k {
	case "user":
		return matrixID, ident.KindUser, true, nil
	case "room":
		return matrixID, ident.KindRoom, true, nil
	}
	return "", 0, false, fmt.Errorf("store: alias %s has unknown kind %q", alias, k)
}

// PutAlias records matrixID <-> alias. It returns ident.ErrAliasTaken if
// alias names a different Matrix ID; recording the same pair again is not
// an error. Giving matrixID an alias other than the one it already has is
// an error too (not ErrAliasTaken).
func (s *Store) PutAlias(ctx context.Context, kind ident.Kind, matrixID, alias string) error {
	k, err := kindName(kind)
	if err != nil {
		return err
	}
	res, err := s.DB.Exec(ctx,
		`INSERT INTO slkmx_aliases (alias, kind, matrix_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		alias, k, matrixID)
	if err != nil {
		return fmt.Errorf("store: putting alias %s for %s: %w", alias, matrixID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 1 {
		return nil
	}
	cur, ok, err := s.LookupAlias(ctx, kind, matrixID)
	switch {
	case err != nil:
		return err
	case !ok:
		return ident.ErrAliasTaken
	case cur == alias:
		return nil
	default:
		return fmt.Errorf("store: %s %s already has alias %s, not %s: %w", k, matrixID, cur, alias, ident.ErrAlreadyMapped)
	}
}

// found reports whether a QueryRow Scan found a row.
func found(err error) bool { return err == nil }

// ignoreNoRows turns sql.ErrNoRows into nil.
func ignoreNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}
