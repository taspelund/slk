// Package store is the Matrix backend's database: one SQLite file per
// account (matrix.db), opened with the pure-Go modernc driver slk already
// uses. It holds mautrix's crypto and state store tables, which session
// migrates, and this backend's own tables, which Open migrates: the sync
// token, rooms, events, TS and alias maps (implementing
// ident.Persistence), receipts, timeline gaps, the media cache index, and
// a full-text index over message text.
//
// # Connection settings
//
// The pool opens every connection with busy_timeout(10000),
// journal_mode(WAL), foreign_keys(1) and synchronous(NORMAL), and with
// _txlock=immediate, which makes the driver start every read-write
// transaction with BEGIN IMMEDIATE. That last one matters: a deferred
// transaction that reads and then writes fails with SQLITE_BUSY (or
// SQLITE_BUSY_SNAPSHOT, once another connection has committed since its
// read) whatever the busy timeout, and mautrix's stores and ident's TS
// allocation both read before writing inside transactions. With
// IMMEDIATE the write lock is taken at BEGIN, where busy_timeout does
// apply, so concurrent writers queue instead of failing
// (TestConcurrentWriters fails within one run without it). The cost is
// that every read-write transaction holds the write lock from its start,
// so keep them short; read-only transactions (sql.TxOptions.ReadOnly)
// still begin deferred. WAL keeps readers from blocking the writer;
// synchronous(NORMAL) is the usual WAL pairing (a power loss can drop the
// last commits but not corrupt the file).
//
// # Migrations
//
// This backend's tables use dbutil's upgrade tables, the mechanism
// mautrix's crypto store (version table crypto_version) and state store
// (mx_version) use on the same database: a Child of DB with its own
// version table, slkmx_version, and SQL files under upgrades/ with
// dbutil's "-- vN -> vM: message" headers. Every table, index and trigger
// is prefixed slkmx_, so it cannot clash with mautrix's crypto_* and mx_*
// names. To change the schema, add upgrades/NN-name.sql ("-- v2 -> v3:
// ...") and update 00-latest-revision.sql to the new shape and its header
// to "-- v0 -> vN", as mautrix does. testdata/v1 is the v1 schema, which
// TestMigrateFromV1 upgrades.
//
// # Schema
//
// upgrades/00-latest-revision.sql is the schema, commented per table.
// There are no foreign keys between this backend's tables: Matrix data
// arrives in any order (events of a room before its state is stored, the
// space parent of a room the account is not in), and refusing it would
// lose data. Events have an explicit INTEGER PRIMARY KEY (seq) beside
// their (room_id, event_id) key so the full-text index's rowids survive
// VACUUM, and a unique (room_id, ts) index as ts_map has. Rooms keep the
// Matrix canonical alias as canonical_alias, to tell it from ident's
// slk alias, and last_event_ts is an slk TS, which sorts across rooms.
//
// # Full-text search
//
// slkmx_events_fts is an FTS5 table with external content (slkmx_events,
// rowid = seq) over each event's body column, kept in step by triggers on
// insert, update and delete. Triggers, rather than explicit methods,
// because every way an event row changes (PutEvent's upsert, RedactEvent,
// DeleteEvent, or a later package's own SQL) then updates the index, and
// the index can never hold text the events table no longer has. A CHECK
// constraint keeps a redacted event's body empty, so the index is exactly
// the body column (FTS5's integrity-check and rebuild hold) and redacted
// events have no terms in it.
//
// # Transactions
//
// Every method runs on DB with the context it is given, so calling them
// inside s.DB.DoTxn(ctx, nil, func(ctx context.Context) error {...}) puts
// them in that transaction. dbutil rewrites $1-style placeholders to
// SQLite's ?1 everywhere in a query, string literals included, so never
// write a literal containing "$<digit>" (event IDs start with "$"): pass
// it as an argument.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"

	"go.mau.fi/util/dbutil"
	_ "modernc.org/sqlite"
)

// VersionTable is the dbutil version table of this backend's schema.
const VersionTable = "slkmx_version"

// dsnParams are the per-connection settings; see the package doc.
const dsnParams = "_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"

//go:embed upgrades/*.sql
var upgradeFS embed.FS

var upgradeTable = dbutil.BuildUpgradeTable().WithFSPath(upgradeFS, "upgrades").Finish()

// Store is an open account database.
type Store struct {
	// DB is the database handle, shared with mautrix's crypto and state
	// stores.
	DB *dbutil.Database
}

// dsn is the modernc DSN for the database file at path. The path is
// escaped because SQLite parses "file:" names as URIs.
func dsn(path string) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + dsnParams
}

// Open opens (creating if needed) the database at path, with foreign keys
// and WAL on, and migrates this backend's tables to the current version.
func Open(ctx context.Context, path string) (*Store, error) {
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("store: wrapping %s: %w", path, err)
	}
	if err := db.Child(VersionTable, upgradeTable, nil).Upgrade(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("store: migrating %s: %w", path, err)
	}
	return &Store{DB: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.DB.Close()
}
