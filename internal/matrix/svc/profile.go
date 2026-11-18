package svc

// WP-Profile: profiles and presence. This file is the work package's; it owns profileState
// (the Backend's profile field) and fillProfile.
//
// Profiles (ProfileService). The UI knows a user by alias; the profile
// dialog gets the alias's MXID as the handle and the display name as both
// names, the other fields empty (Matrix profiles have none of them). The
// name comes from the room model (a member event's displayname, the
// user's global one) when it has one, with no request; otherwise from
// GET /profile, whose answer profileState caches for profileTTL. A
// profile the server will not show (M_NOT_FOUND, M_FORBIDDEN: no shared
// room) is the localpart, as the room model names an unknown user.
//
// Presence (PresenceService). Active and Away set the account's Matrix
// presence (online, unavailable) with PUT /presence. (Away does not hold
// yet while the session syncs: its /sync sends no set_presence, which
// marks the account online again on every sync.) The UI applies a
// choice before calling SetStatus, so a failed request is undone with a
// StatusChangeMsg back to the last presence the server took, and a
// toast. Matrix has no do-not-disturb and no custom status: a snooze is
// undone the same way (DND off) with a toast saying so, and the custom
// status presets only toast (the UI shows those on the server's echo,
// so nothing to undo). EndDND does nothing: DND is never on.
// SendTyping is m.typing on the room (PUT /typing).
//
// SetStatus runs on the UI's Update goroutine and must not block, so the
// request and the messages it answers with go from a goroutine through
// the function SetUISend gives (tea.Program.Send).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/debuglog"
	"github.com/gammons/slk/internal/ui"
)

const (
	// profileTTL is how long a fetched /profile answer is reused.
	profileTTL = 10 * time.Minute
	// profileCacheMax bounds the cache; the oldest entry goes first.
	profileCacheMax = 256
	// presenceTimeout bounds one PUT /presence.
	presenceTimeout = 15 * time.Second
	// typingTimeout is how long one typing notice lasts on the server; the
	// UI repeats SendTyping every typingThrottle (3s) while the user
	// types.
	typingTimeout = 5 * time.Second
	// typingRequestTimeout bounds one PUT /typing.
	typingRequestTimeout = 10 * time.Second
)

// Toasts for presence choices Matrix has no equivalent of.
const (
	toastNoDND          = "Matrix has no Do Not Disturb; notifications are unchanged"
	toastNoCustomStatus = "Matrix has no custom status"
)

// profileState is WP-Profile's state on a Backend.
type profileState struct {
	mu sync.Mutex
	// cache is /profile answers by user.
	cache map[id.UserID]cachedProfile
	// send delivers messages to the UI (SetUISend); nil drops them.
	send func(tea.Msg)
	// want is the presence the user chose last ("active" or "away");
	// have the last one the server took (or the default, "active": a
	// syncing client is online).
	want, have string
	// putMu serializes PUT /presence, each sending want as it is then,
	// so the last request carries the latest choice.
	putMu sync.Mutex
}

// cachedProfile is one /profile answer.
type cachedProfile struct {
	name string
	at   time.Time
}

func newProfileState() *profileState {
	return &profileState{cache: map[id.UserID]cachedProfile{}, want: "active", have: "active"}
}

// SetUISend sets how the backend sends messages to the UI outside Run
// (tea.Program.Send): presence failures and the toasts of SetStatus.
// Until it is set they are dropped.
func (b *Backend) SetUISend(send func(tea.Msg)) {
	s := b.profile
	s.mu.Lock()
	s.send = send
	s.mu.Unlock()
}

// fillProfile sets WP-Profile's ports on p.
func (b *Backend) fillProfile(p *ports) {
	p.profiles.Profile = b.userProfile
	p.profiles.Cached = b.cachedProfile
	p.presence = core.NewPresenceService(b.setStatus, b.sendTyping)
}

// userProfile is ProfileService.Profile.
func (b *Backend) userProfile(ctx context.Context, teamID, alias string) (core.UserProfile, error) {
	user, err := b.profileUser(teamID, alias)
	if err != nil {
		return core.UserProfile{}, err
	}
	if name, ok := b.modelName(user); ok {
		return b.makeProfile(alias, user, name), nil
	}
	if name, ok := b.profile.cached(user, time.Now()); ok {
		return b.makeProfile(alias, user, name), nil
	}
	if b.client == nil {
		return b.makeProfile(alias, user, localpartOf(user)), nil
	}
	resp, err := b.client.GetProfile(ctx, user)
	var name string
	switch {
	case err == nil:
		name = resp.DisplayName
	case errors.Is(err, mautrix.MNotFound) || errors.Is(err, mautrix.MForbidden):
	default:
		return core.UserProfile{}, fmt.Errorf("profile of %s: %w", user, err)
	}
	if name == "" {
		name = localpartOf(user)
	}
	b.profile.store(user, name, time.Now())
	return b.makeProfile(alias, user, name), nil
}

// cachedProfile is ProfileService.Cached: the room model's name, else a
// fresh /profile answer.
func (b *Backend) cachedProfile(teamID, alias string) (core.UserProfile, bool) {
	user, err := b.profileUser(teamID, alias)
	if err != nil {
		return core.UserProfile{}, false
	}
	name, ok := b.modelName(user)
	if !ok {
		name, ok = b.profile.cached(user, time.Now())
	}
	if !ok {
		return core.UserProfile{}, false
	}
	return b.makeProfile(alias, user, name), true
}

// profileUser is the MXID of a user alias in this account's workspace.
func (b *Backend) profileUser(teamID, alias string) (id.UserID, error) {
	if teamID != b.teamID {
		return "", fmt.Errorf("svc: unknown workspace %q", teamID)
	}
	user, ok := b.rooms.Resolver().MXID(alias)
	if !ok {
		return "", fmt.Errorf("svc: unknown user %q", alias)
	}
	return user, nil
}

// modelName is the room model's name for user, false when the model only
// has the localpart it falls back to.
func (b *Backend) modelName(user id.UserID) (string, bool) {
	name := b.rooms.DisplayName("", user)
	return name, name != "" && name != localpartOf(user)
}

func (b *Backend) makeProfile(alias string, user id.UserID, name string) core.UserProfile {
	return core.UserProfile{UserID: alias, TeamID: b.teamID, Handle: string(user), RealName: name, DisplayName: name}
}

// localpartOf is the name the room model gives a user it has no name for
// (rooms' localpart).
func localpartOf(user id.UserID) string {
	lp, _, err := user.Parse()
	if err != nil || lp == "" {
		return strings.TrimPrefix(string(user), "@")
	}
	return lp
}

// cached is user's /profile name, if fetched within profileTTL of now.
func (s *profileState) cached(user id.UserID, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cache[user]
	if !ok || now.Sub(c.at) > profileTTL {
		return "", false
	}
	return c.name, true
}

// store caches user's /profile name, dropping the oldest entry when full.
func (s *profileState) store(user id.UserID, name string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cache[user]; !ok && len(s.cache) >= profileCacheMax {
		var oldest id.UserID
		for u, c := range s.cache {
			if oldest == "" || c.at.Before(s.cache[oldest].at) {
				oldest = u
			}
		}
		delete(s.cache, oldest)
	}
	s.cache[user] = cachedProfile{name: name, at: now}
}

// setStatus is PresenceService.SetStatus. It runs on the UI goroutine.
func (b *Backend) setStatus(action core.PresenceAction, _ int) {
	s := b.profile
	switch action {
	case core.PresenceSetActive, core.PresenceSetAway:
		want := "active"
		if action == core.PresenceSetAway {
			want = "away"
		}
		s.mu.Lock()
		s.want = want
		s.mu.Unlock()
		go b.putPresence()
	case core.PresenceSnooze:
		s.mu.Lock()
		want := s.want
		s.mu.Unlock()
		b.uiSend(ui.StatusChangeMsg{TeamID: b.teamID, Presence: want}, ui.ToastMsg{Text: toastNoDND})
	case core.PresenceSetWFH, core.PresenceSetVacation, core.PresenceClearStatus:
		b.uiSend(ui.ToastMsg{Text: toastNoCustomStatus})
	}
}

// putPresence sets the account's presence to the latest choice. On
// failure it puts the UI back to the presence the server has, unless a
// later choice is still on its way.
func (b *Backend) putPresence() {
	s := b.profile
	s.putMu.Lock()
	defer s.putMu.Unlock()
	s.mu.Lock()
	want := s.want
	s.mu.Unlock()
	presence := event.PresenceOnline
	if want == "away" {
		presence = event.PresenceUnavailable
	}
	var err error
	if b.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), presenceTimeout)
		err = b.client.SetPresence(ctx, mautrix.ReqPresence{Presence: presence})
		cancel()
	}
	s.mu.Lock()
	if err == nil {
		s.have = want
	}
	have, latest := s.have, s.want == want
	s.mu.Unlock()
	if err == nil || !latest {
		return
	}
	debuglog.General("matrix: set presence %s: %v", presence, err)
	b.uiSend(ui.StatusChangeMsg{TeamID: b.teamID, Presence: have}, ui.ToastMsg{Text: "Couldn't set presence: " + err.Error()})
}

// uiSend sends msgs to the UI in order, from a new goroutine:
// tea.Program.Send blocks until the Update goroutine, which may be the
// caller, takes the message.
func (b *Backend) uiSend(msgs ...tea.Msg) {
	s := b.profile
	s.mu.Lock()
	send := s.send
	s.mu.Unlock()
	if send == nil {
		debuglog.General("matrix: no UI to send %d messages to", len(msgs))
		return
	}
	go func() {
		for _, m := range msgs {
			send(m)
		}
	}()
}

// sendTyping is PresenceService.SendTyping: a typing notice in the room
// alias names. It runs off the UI goroutine.
func (b *Backend) sendTyping(alias string) {
	room, ok := b.rooms.Room(alias)
	if !ok || b.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), typingRequestTimeout)
	defer cancel()
	if _, err := b.client.UserTyping(ctx, room.ID, true, typingTimeout); err != nil {
		debuglog.General("matrix: typing in %s: %v", room.ID, err)
	}
}
