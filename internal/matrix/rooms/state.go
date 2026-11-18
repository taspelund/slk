package rooms

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/text"
)

// Section IDs of the two built-in sections. A space's section ID is the
// space's room alias ("C…"), which cannot collide with these.
const (
	sectionRooms = "rooms"
	sectionDMs   = "dms"
)

// maxHeroes is how many members name a room without a name, as the spec's
// m.heroes (servers send up to 5).
const maxHeroes = 5

// member is a user's membership in a room, as its latest member event
// says.
type member struct {
	name       string
	membership event.Membership
}

// room is one room in memory. A room the Model has published (in
// Model.rooms) is never modified again: a writer clones it (batch.room),
// changes the clone and publishes that, so readers holding the old value
// need no lock on it.
//
// row is what the store holds; the other fields are the inputs the row's
// name and kind are computed from, which saveInputs copies into the row
// (members are in mautrix's state store on the same database instead),
// and the read state's, from slkmx_receipts and slkmx_events.
type room struct {
	row   store.Room
	alias string

	// explicitName is the m.room.name content's name, nil when no
	// m.room.name was seen (or, after a restart, when the stored name is
	// the computed one).
	explicitName *string
	// heroes and the member counts are from the sync summary.
	heroes            []id.UserID
	joinedN, invitedN int
	haveCounts        bool
	joinRule          event.JoinRule

	// direct says the room is in m.direct, with directUsers the users it
	// is listed under. minOthers is a lower bound on the other members
	// restored from a stored group_dm kind, until m.direct is seen again.
	direct      bool
	directUsers []id.UserID
	minOthers   int
	// inviteDirect is an invite whose member event has is_direct, from
	// inviter.
	inviteDirect bool
	inviter      id.UserID

	members map[id.UserID]member
	// receipts are the account's own read receipts, by thread_id ("",
	// "main" or a thread root).
	receipts map[string]store.Receipt
	// candTS is the newest event that would make the room unread
	// (store.LatestUnreadCandidate), ownTS the newest one the account
	// sent.
	candTS, ownTS string
}

func newRoom(rid id.RoomID, alias string) *room {
	return &room{
		row:      store.Room{ID: rid, Kind: store.RoomPrivate, JoinState: store.Joined},
		alias:    alias,
		members:  map[id.UserID]member{},
		receipts: map[string]store.Receipt{},
	}
}

// clone returns a copy of r that shares nothing mutable with it.
func (r *room) clone() *room {
	c := *r
	if r.explicitName != nil {
		n := *r.explicitName
		c.explicitName = &n
	}
	c.heroes = slices.Clone(r.heroes)
	c.directUsers = slices.Clone(r.directUsers)
	c.members = maps.Clone(r.members)
	c.receipts = maps.Clone(r.receipts)
	return &c
}

// saveInputs copies the name and kind inputs into the row, which stores
// them.
func (r *room) saveInputs() {
	row := &r.row
	row.InputsSaved = true
	row.HasNameEvent, row.NameEvent = r.explicitName != nil, ""
	if r.explicitName != nil {
		row.NameEvent = *r.explicitName
	}
	row.Heroes = nilIfEmpty(r.heroes)
	row.HaveMemberCounts, row.JoinedMemberCount, row.InvitedMemberCount = r.haveCounts, r.joinedN, r.invitedN
	if !r.haveCounts {
		row.JoinedMemberCount, row.InvitedMemberCount = 0, 0
	}
	row.JoinRule = string(r.joinRule)
	row.DirectUsers = nil
	if r.direct {
		row.DirectUsers = nilIfEmpty(r.directUsers)
	}
}

// loadInputs sets the name and kind inputs from a row that has them
// (InputsSaved).
func (r *room) loadInputs() {
	row := r.row
	r.explicitName = nil
	if row.HasNameEvent {
		n := row.NameEvent
		r.explicitName = &n
	}
	r.heroes = slices.Clone(row.Heroes)
	r.haveCounts, r.joinedN, r.invitedN = row.HaveMemberCounts, row.JoinedMemberCount, row.InvitedMemberCount
	r.joinRule = event.JoinRule(row.JoinRule)
	r.direct, r.directUsers = len(row.DirectUsers) > 0, slices.Clone(row.DirectUsers)
	if row.JoinState == store.Invited && row.Kind == store.RoomDM {
		r.inviteDirect, r.inviter = true, row.DMUserID
	}
}

func nilIfEmpty(us []id.UserID) []id.UserID {
	if len(us) == 0 {
		return nil
	}
	return slices.Clone(us)
}

// nameLookup resolves display names while computing room names.
type nameLookup func(rid id.RoomID, user id.UserID) string

// joinedOthers lists the joined or invited members other than self,
// sorted.
func (r *room) joinedOthers(self id.UserID) []id.UserID {
	var out []id.UserID
	for u, mem := range r.members {
		if u != self && (mem.membership == event.MembershipJoin || mem.membership == event.MembershipInvite) {
			out = append(out, u)
		}
	}
	slices.Sort(out)
	return out
}

// otherCount is how many members besides self the room has: the summary's
// counts when the server sent them, else what is known locally.
func (r *room) otherCount(self id.UserID) int {
	if r.haveCounts {
		return max(r.joinedN+r.invitedN-1, 0)
	}
	return max(len(r.joinedOthers(self)), len(r.directUsers), r.minOthers)
}

// kind is the room's sidebar kind and, for a dm, the other user.
func (r *room) kind(self id.UserID) (store.RoomKind, id.UserID) {
	switch {
	case r.direct:
		if r.otherCount(self) <= 1 {
			var u id.UserID
			if len(r.directUsers) > 0 {
				u = r.directUsers[0]
			} else if others := r.joinedOthers(self); len(others) > 0 {
				u = others[0]
			} else {
				u = r.row.DMUserID
			}
			return store.RoomDM, u
		}
		return store.RoomGroupDM, ""
	case r.row.JoinState == store.Invited && r.inviteDirect:
		return store.RoomDM, r.inviter
	case r.joinRule == event.JoinRulePublic:
		return store.RoomChannel, ""
	}
	return store.RoomPrivate, ""
}

// name computes the room's name: m.room.name, else the canonical alias,
// else for a dm the other user's display name, else the spec's
// room-name algorithm over the heroes.
func (r *room) name(self id.UserID, kind store.RoomKind, dmUser id.UserID, dn nameLookup) string {
	if r.explicitName != nil && *r.explicitName != "" {
		return *r.explicitName
	}
	if r.row.CanonicalAlias != "" {
		return string(r.row.CanonicalAlias)
	}
	if kind == store.RoomDM && dmUser != "" {
		return dn(r.row.ID, dmUser)
	}
	heroes := r.heroes
	if len(heroes) == 0 {
		heroes = r.joinedOthers(self)
		if len(heroes) > maxHeroes {
			heroes = heroes[:maxHeroes]
		}
	}
	names := make([]string, 0, len(heroes))
	for _, h := range heroes {
		names = append(names, dn(r.row.ID, h))
	}
	others := r.otherCount(self)
	switch {
	case len(names) == 0:
		return "Empty Room"
	case others == 0:
		return "Empty Room (was " + strings.Join(names, ", ") + ")"
	case others > len(names):
		n := others - len(names)
		if n == 1 {
			return strings.Join(names, ", ") + " and 1 other"
		}
		return fmt.Sprintf("%s and %d others", strings.Join(names, ", "), n)
	}
	return strings.Join(names, ", ")
}

// mainReceipt is the later of the account's unthreaded and "main"
// receipts.
func (r *room) mainReceipt() (store.Receipt, bool) {
	a, okA := r.receipts[""]
	b, okB := r.receipts[string(event.ReadReceiptThreadMain)]
	switch {
	case okA && okB:
		if b.TS > a.TS {
			return b, true
		}
		return a, true
	case okB:
		return b, true
	}
	return a, okA
}

// readState is the room's read state; see the package doc.
func (r *room) readState() core.ReadState {
	rec, ok := r.mainReceipt()
	rs := core.ReadState{LastReadTS: rec.TS, MentionCount: r.row.HighlightCount}
	switch {
	case r.row.MarkedUnread:
		rs.HasUnread = true
	case r.candTS == "":
	case ok && rec.TS == "":
		// The receipt is on an event this client has not stored (older
		// than what it has), so the server's count decides.
		rs.HasUnread = r.row.NotificationCount > 0 && r.candTS > r.ownTS
	default:
		rs.HasUnread = r.candTS > max(rec.TS, r.ownTS)
	}
	return rs
}

// sidebar reports whether the room is a sidebar row: joined, not a space.
func (r *room) sidebar() bool {
	return r.row.JoinState == store.Joined && !r.row.IsSpace
}

// sectionOf returns the section ID of r: dms for direct chats, else the
// room's first joined parent space (by the m.space.child order, rooms
// without an order last, then by the space's name and ID), else rooms.
// get looks rooms up, edges are the space edges by child.
func sectionOf(r *room, get func(id.RoomID) *room, edges map[id.RoomID]map[id.RoomID]string) string {
	if r.row.Kind == store.RoomDM || r.row.Kind == store.RoomGroupDM {
		return sectionDMs
	}
	var best *room
	var bestOrder string
	for sid, order := range edges[r.row.ID] {
		s := get(sid)
		if s == nil || !s.row.IsSpace || s.row.JoinState != store.Joined {
			continue
		}
		if best == nil || spaceBefore(order, s, bestOrder, best) {
			best, bestOrder = s, order
		}
	}
	if best == nil {
		return sectionRooms
	}
	return best.alias
}

// spaceBefore orders a room's parent spaces: by order (an empty order
// last), then name, then ID.
func spaceBefore(oa string, a *room, ob string, b *room) bool {
	if (oa == "") != (ob == "") {
		return oa != ""
	}
	if oa != ob {
		return oa < ob
	}
	na, nb := text.Fold(a.row.Name), text.Fold(b.row.Name)
	if na != nb {
		return na < nb
	}
	return a.row.ID < b.row.ID
}

// view is what of a room the sidebar shows; Changes.Rooms lists the rooms
// whose view changed.
type view struct {
	name, topic string
	kind        store.RoomKind
	join        store.JoinState
	section     string
	dmUser      id.UserID
	encrypted   bool
	space       bool
}

func (r *room) view(section string) view {
	return view{
		name: r.row.Name, topic: r.row.Topic, kind: r.row.Kind, join: r.row.JoinState,
		section: section, dmUser: r.row.DMUserID, encrypted: r.row.Encrypted, space: r.row.IsSpace,
	}
}

// localpart is the fallback display name of user.
func localpart(user id.UserID) string {
	lp, _, err := user.Parse()
	if err != nil || lp == "" {
		return strings.TrimPrefix(string(user), "@")
	}
	return lp
}
