// Package svc implements slk's internal/core ports for a Matrix account and
// turns what the account syncs into live ui messages, the same contract
// internal/demo implements for the demo.
//
// It reads the account database (store) and the room model (rooms), and
// makes requests through the session's client. It never writes the room or
// timeline tables itself: events it fetches go through rooms.Model.Backfill
// or FillGap, synced ones through Apply and ApplyDecrypted (Run), and
// events it sends come back through /sync.
//
// Wiring, in cmd/slk: New, then Install on the App, SetNotifications
// (how new messages notify; before Run), then Run on the session's events
// in a goroutine and StartupMsgs. Run holds after the first sync (or the
// first failed one) until StartupMsgs has built the workspace from the
// model, and the batch that snapshot holds sends nothing, so sending
// StartupMsgs' messages before letting Run's sends through delivers the
// workspace first and loses nothing (live.go shows the gate).
//
// The port implementations are split by work package, each filling its part
// of a ports value:
//
//	read.go     fillRead     channel history, threads, unread, search, avatars (WP-Read)
//	write.go    fillWrite    send, edit, delete, reactions, receipts, joins (WP-Write)
//	files.go    fillFiles    uploads, downloads, images (WP-Media)
//	profile.go  fillProfile  profiles, presence (WP-Profile)
//	live.go     StartupMsgs, Run, SetNotifications: sync batches -> ui messages (WP-Live)
package svc

import (
	"errors"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui"
)

// Deps are what a Backend needs. Account is the account name given to
// --matrix; it names the workspace in the rail. Dir is the account's
// directory (session.json, matrix.db), where WP-Media keeps its file
// cache. ReadOnly leaves out every port that changes the account (sends,
// edits, reactions, read receipts, joins, presence, typing, uploads), for
// trying an account without touching it. TimestampFormat is the
// Go time layout of a message's header time, [appearance]
// timestamp_format ("" for its default, "3:04 PM").
type Deps struct {
	Account         string
	Dir             string
	ReadOnly        bool
	Session         *session.Session
	Store           *store.Store
	IDs             *ident.Mapper
	Rooms           *rooms.Model
	TimestampFormat string
}

// TeamID is the workspace ID the UI knows this account by.
func (b *Backend) TeamID() string { return b.teamID }

// ports collects the implementations each work package fills in. Ports
// left unset fall back to internal/core's documented no-op adapters.
type ports struct {
	channels  core.ChannelServiceFuncs
	threads   core.ThreadServiceFuncs
	messages  core.MessageServiceFuncs
	search    core.SearchServiceFuncs
	profiles  core.ProfileServiceFuncs
	reactions core.ReactionService
	unread    core.UnreadService
	workspace core.WorkspaceService
	avatars   core.AvatarService
	files     core.FileService
	presence  core.PresenceService
	images    core.ImageFetcher
}

// errReadOnly is what a write a read-only backend refuses reports.
var errReadOnly = errors.New("read-only: this session does not change the account")

// Install wires the backend's ports into app, as internal/demo's Install
// does.
func (b *Backend) Install(app *ui.App) {
	p := b.ports()
	app.SetChannelService(core.NewChannelService(p.channels))
	app.SetThreadService(core.NewThreadService(p.threads))
	app.SetMessageService(core.NewMessageService(p.messages))
	app.SetSearchService(core.NewSearchService(p.search))
	app.SetProfileService(core.NewProfileService(p.profiles))
	if p.reactions != nil {
		app.SetReactionService(p.reactions)
	}
	if p.unread != nil {
		app.SetUnreadService(p.unread)
	}
	if p.workspace != nil {
		app.SetWorkspaceService(p.workspace)
	}
	if p.avatars != nil {
		app.SetAvatarService(p.avatars)
	}
	if p.files != nil {
		app.SetFileService(p.files)
	}
	if p.presence != nil {
		app.SetPresenceService(p.presence)
	}
	if p.images != nil {
		app.SetImageFetcher(p.images)
	}
}

// ports builds the backend's port implementations; ReadOnly leaves out
// the ones that change the account.
func (b *Backend) ports() ports {
	var p ports
	b.fillRead(&p)
	if !b.d.ReadOnly {
		b.fillWrite(&p)
	}
	b.fillFiles(&p)
	b.fillProfile(&p)
	if b.d.ReadOnly {
		p.presence = nil
		if p.files != nil {
			p.files = core.NewFileService(func(string, string, string, []core.PendingAttachment) core.Cmd {
				return func() core.Msg { return ui.UploadResultMsg{Err: errReadOnly} }
			}, p.files.Download)
		}
	}
	return p
}
