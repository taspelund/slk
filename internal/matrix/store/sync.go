package store

import (
	"context"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

// Store is the mautrix client's sync store. A database holds one account,
// so the user ID arguments are ignored.
var _ mautrix.SyncStore = (*Store)(nil)

// SaveFilterID records the /sync filter ID.
func (s *Store) SaveFilterID(ctx context.Context, _ id.UserID, filterID string) error {
	_, err := s.DB.Exec(ctx, `UPDATE slkmx_sync_state SET filter_id = $1 WHERE id = 0`, filterID)
	return err
}

// LoadFilterID returns the /sync filter ID, "" if none was saved.
func (s *Store) LoadFilterID(ctx context.Context, _ id.UserID) (string, error) {
	var filterID string
	err := s.DB.QueryRow(ctx, `SELECT filter_id FROM slkmx_sync_state WHERE id = 0`).Scan(&filterID)
	return filterID, err
}

// SaveNextBatch records the token the next /sync starts from.
func (s *Store) SaveNextBatch(ctx context.Context, _ id.UserID, nextBatch string) error {
	_, err := s.DB.Exec(ctx, `UPDATE slkmx_sync_state SET next_batch = $1 WHERE id = 0`, nextBatch)
	return err
}

// LoadNextBatch returns the token the next /sync starts from, "" before
// the first sync.
func (s *Store) LoadNextBatch(ctx context.Context, _ id.UserID) (string, error) {
	var nextBatch string
	err := s.DB.QueryRow(ctx, `SELECT next_batch FROM slkmx_sync_state WHERE id = 0`).Scan(&nextBatch)
	return nextBatch, err
}
