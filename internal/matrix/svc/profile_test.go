package svc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/ui"
)

const stranger id.UserID = "@stranger:example.org"

// profilePorts fills WP-Profile's ports of h's backend.
func profilePorts(h *harness) ports {
	var p ports
	h.b.fillProfile(&p)
	return p
}

// noProfileServer is a homeserver every request to fails the test.
func noProfileServer(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func TestProfileOfARoomMemberComesFromTheModel(t *testing.T) {
	h := newHarness(t, noProfileServer(t))
	p := profilePorts(h)
	alias := h.res().UserAlias(alice)
	want := core.UserProfile{UserID: alias, TeamID: h.b.TeamID(), Handle: "@alice:example.org", RealName: "Alice Liddell", DisplayName: "Alice Liddell"}
	got, err := p.profiles.Profile(context.Background(), h.b.TeamID(), alias)
	if err != nil || got != want {
		t.Fatalf("Profile = %+v, %v; want %+v", got, err, want)
	}
	if got, ok := p.profiles.Cached(h.b.TeamID(), alias); !ok || got != want {
		t.Errorf("Cached = %+v, %v", got, ok)
	}
}

func TestProfileOfAStrangerIsFetchedOnceAndCached(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/_matrix/client/v3/profile/"+string(stranger) {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, map[string]any{"displayname": "Carol C", "avatar_url": "mxc://example.org/abc"})
	})
	p := profilePorts(h)
	alias := h.res().UserAlias(stranger)
	if _, ok := p.profiles.Cached(h.b.TeamID(), alias); ok {
		t.Errorf("Cached before any fetch")
	}
	want := core.UserProfile{UserID: alias, TeamID: h.b.TeamID(), Handle: string(stranger), RealName: "Carol C", DisplayName: "Carol C"}
	for range 2 {
		got, err := p.profiles.Profile(context.Background(), h.b.TeamID(), alias)
		if err != nil || got != want {
			t.Fatalf("Profile = %+v, %v; want %+v", got, err, want)
		}
	}
	if got, ok := p.profiles.Cached(h.b.TeamID(), alias); !ok || got != want {
		t.Errorf("Cached = %+v, %v", got, ok)
	}
	if n := len(h.reqs()); n != 1 {
		t.Errorf("%d requests, want 1: %v", n, h.reqs())
	}
}

func TestProfileCacheExpires(t *testing.T) {
	s := newProfileState()
	now := time.Now()
	s.store(stranger, "Carol", now)
	if name, ok := s.cached(stranger, now.Add(profileTTL)); !ok || name != "Carol" {
		t.Errorf("cached at the TTL = %q, %v", name, ok)
	}
	if _, ok := s.cached(stranger, now.Add(profileTTL+time.Second)); ok {
		t.Errorf("cached after the TTL")
	}
}

func TestProfileCacheDropsTheOldestWhenFull(t *testing.T) {
	s := newProfileState()
	now := time.Now()
	for i := range profileCacheMax {
		s.store(id.UserID("@u"+itoa3(i)+":example.org"), "u", now.Add(time.Duration(i)*time.Second))
	}
	s.store(stranger, "Carol", now.Add(time.Hour))
	if len(s.cache) != profileCacheMax {
		t.Fatalf("cache holds %d, want %d", len(s.cache), profileCacheMax)
	}
	if _, ok := s.cache["@u000:example.org"]; ok {
		t.Errorf("the oldest entry was kept")
	}
	if _, ok := s.cache[stranger]; !ok {
		t.Errorf("the new entry was not stored")
	}
}

func TestProfileTheServerWillNotShowIsTheLocalpart(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(t, w, map[string]any{"errcode": "M_NOT_FOUND", "error": "Profile was not found"})
	})
	p := profilePorts(h)
	got, err := p.profiles.Profile(context.Background(), h.b.TeamID(), h.res().UserAlias(stranger))
	if err != nil || got.DisplayName != "stranger" || got.RealName != "stranger" || got.Handle != string(stranger) {
		t.Errorf("Profile = %+v, %v", got, err)
	}
}

func TestProfileFailsOnAServerError(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(t, w, map[string]any{"errcode": "M_UNKNOWN", "error": "boom"})
	})
	p := profilePorts(h)
	alias := h.res().UserAlias(stranger)
	if got, err := p.profiles.Profile(context.Background(), h.b.TeamID(), alias); err == nil {
		t.Errorf("Profile = %+v, want an error", got)
	}
	if _, ok := p.profiles.Cached(h.b.TeamID(), alias); ok {
		t.Errorf("a failed fetch was cached")
	}
}

func TestProfileOfAnUnknownAliasOrWorkspaceFails(t *testing.T) {
	h := newHarness(t, noProfileServer(t))
	p := profilePorts(h)
	if _, err := p.profiles.Profile(context.Background(), h.b.TeamID(), "UNOBODY0000"); err == nil {
		t.Errorf("Profile of an unknown alias succeeded")
	}
	if _, err := p.profiles.Profile(context.Background(), "T1", h.res().UserAlias(alice)); err == nil {
		t.Errorf("Profile in another workspace succeeded")
	}
	if _, ok := p.profiles.Cached("T1", h.res().UserAlias(alice)); ok {
		t.Errorf("Cached in another workspace succeeded")
	}
}

// presenceServer is a homeserver answering PUT /presence with status
// (and an error body when it is not 200), recording each body's presence
// on the channel it returns.
func presenceServer(t *testing.T, status int) (http.HandlerFunc, <-chan string) {
	puts := make(chan string, 10)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/_matrix/client/v3/presence/"+string(self)+"/status" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var body struct{ Presence string }
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("body %s: %v", raw, err)
		}
		w.WriteHeader(status)
		if status != http.StatusOK {
			writeJSON(t, w, map[string]any{"errcode": "M_UNKNOWN", "error": "boom"})
		} else {
			writeJSON(t, w, map[string]any{})
		}
		puts <- body.Presence
	}, puts
}

// profileUIMsgs sets h's backend to send UI messages to the channel it returns.
func profileUIMsgs(h *harness) <-chan tea.Msg {
	ch := make(chan tea.Msg, 10)
	h.b.SetUISend(func(m tea.Msg) { ch <- m })
	return ch
}

func awaitOne[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	var zero T
	return zero
}

func TestSetStatusAwayAndActiveSetMatrixPresence(t *testing.T) {
	handler, puts := presenceServer(t, http.StatusOK)
	h := newHarness(t, handler)
	msgs := profileUIMsgs(h)
	p := profilePorts(h)
	p.presence.SetStatus(core.PresenceSetAway, 0)
	if got := awaitOne(t, puts); got != "unavailable" {
		t.Errorf("away put %q", got)
	}
	p.presence.SetStatus(core.PresenceSetActive, 0)
	if got := awaitOne(t, puts); got != "online" {
		t.Errorf("active put %q", got)
	}
	select {
	case m := <-msgs:
		t.Errorf("sent %#v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSetStatusFailureRestoresTheServersPresence(t *testing.T) {
	handler, puts := presenceServer(t, http.StatusInternalServerError)
	h := newHarness(t, handler)
	msgs := profileUIMsgs(h)
	p := profilePorts(h)
	p.presence.SetStatus(core.PresenceSetAway, 0)
	awaitOne(t, puts)
	if m := awaitOne(t, msgs); m != (ui.StatusChangeMsg{TeamID: h.b.TeamID(), Presence: "active"}) {
		t.Errorf("first message %#v", m)
	}
	if m, ok := awaitOne(t, msgs).(ui.ToastMsg); !ok || m.Text == "" {
		t.Errorf("second message %#v", m)
	}
}

func TestSnoozeIsUndoneWithAToast(t *testing.T) {
	h := newHarness(t, noProfileServer(t))
	msgs := profileUIMsgs(h)
	p := profilePorts(h)
	p.presence.SetStatus(core.PresenceSnooze, 30)
	got := []tea.Msg{awaitOne(t, msgs), awaitOne(t, msgs)}
	want := []tea.Msg{ui.StatusChangeMsg{TeamID: h.b.TeamID(), Presence: "active"}, ui.ToastMsg{Text: toastNoDND}}
	if !slices.Equal(got, want) {
		t.Errorf("sent %#v, want %#v", got, want)
	}
	p.presence.SetStatus(core.PresenceSetWFH, 0)
	if m := awaitOne(t, msgs); m != (ui.ToastMsg{Text: toastNoCustomStatus}) {
		t.Errorf("WFH sent %#v", m)
	}
	p.presence.SetStatus(core.PresenceEndDND, 0)
	select {
	case m := <-msgs:
		t.Errorf("EndDND sent %#v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSendTypingTypesInTheRoom(t *testing.T) {
	type typing struct {
		Typing  bool
		Timeout int64
	}
	got := make(chan typing, 1)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/_matrix/client/v3/rooms/"+string(general)+"/typing/"+string(self) {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var body typing
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		writeJSON(t, w, map[string]any{})
		got <- body
	})
	p := profilePorts(h)
	p.presence.SendTyping(h.alias)
	if b := awaitOne(t, got); b != (typing{true, typingTimeout.Milliseconds()}) {
		t.Errorf("typing body %+v", b)
	}
	p.presence.SendTyping("CNOSUCHROOM")
	if n := len(h.reqs()); n != 1 {
		t.Errorf("%d requests, want 1: %v", n, h.reqs())
	}
}
