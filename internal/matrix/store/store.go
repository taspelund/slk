// Package store is the Matrix backend's database: one SQLite file per
// account (matrix.db), opened with the pure-Go modernc driver slk already
// uses. It holds mautrix's crypto and state store tables, which session
// migrates, and this backend's own tables, which Open migrates: the sync
// token, rooms, events, TS and alias maps (implementing
// ident.Persistence), receipts, timeline gaps, the media cache index, and
// a full-text index over message text.
package store

import (
	"context"

	"go.mau.fi/util/dbutil"
)

// Store is an open account database.
type Store struct {
	// DB is the database handle, shared with mautrix's crypto and state
	// stores.
	DB *dbutil.Database
}

// Open opens (creating if needed) the database at path, with foreign keys
// and WAL on, and migrates this backend's tables to the current version.
func Open(ctx context.Context, path string) (*Store, error) {
	panic("store: Open not implemented (WP-Store)")
}

// Close closes the database.
func (s *Store) Close() error {
	panic("store: Close not implemented (WP-Store)")
}
