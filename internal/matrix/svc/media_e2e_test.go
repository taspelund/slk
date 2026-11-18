//go:build matrixe2e

package svc

import (
	"bytes"
	"context"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	imgpkg "github.com/gammons/slk/internal/image"
	"github.com/gammons/slk/internal/matrix/e2e"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui"
	"github.com/gammons/slk/internal/ui/imgrender"
	"github.com/gammons/slk/internal/ui/messages"
)

// TestMediaAgainstHomeserver has alice upload a picture into an encrypted
// room through FileService, bob download and decrypt it, alice download
// her own and bob's back, and bob's picture draw in a messages pane
// through the image fetcher.
func TestMediaAgainstHomeserver(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	bob := e2e.NewPeer(t, srv, "bob")
	ctx := e2e.Ctx(t)

	creds, err := session.Login(ctx, srv.URL, aliceID.String(), password, "slk (media e2e)")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "matrix.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ids := ident.New(st)
	alice := startSyncing(t, creds, st, ids)
	alice.b.d.Dir = t.TempDir()
	var p ports
	alice.b.fillFiles(&p)
	if p.files == nil || p.images == nil {
		t.Fatal("no media ports")
	}

	room := bob.CreateRoom(t, e2e.RoomOptions{Name: "pictures", Encrypted: true, Invite: []id.UserID{aliceID}})
	if _, err := alice.b.client.JoinRoomByID(ctx, room); err != nil {
		t.Fatalf("join: %v", err)
	}
	bob.WaitMember(t, room, aliceID, event.MembershipJoin)
	e2e.Eventually(t, "alice's model has the encrypted room", func() bool {
		r, ok := alice.m.RoomByID(room)
		return ok && r.Encrypted
	})
	alias := alice.m.Resolver().RoomAlias(room)
	stored := func(ev id.EventID) bool {
		e, ok, err := st.GetEvent(context.Background(), room, ev)
		return err == nil && ok && e.Type == event.EventMessage.Type
	}
	itemOf := func(ev id.EventID) core.MessageItem {
		t.Helper()
		e2e.Eventually(t, "alice stores "+string(ev), func() bool { return stored(ev) })
		ts, ok, err := ids.TS(ctx, room, ev)
		if err != nil || !ok {
			t.Fatalf("TS of %s: %v %v", ev, ok, err)
		}
		items, err := alice.b.page(ctx, room, "")
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		it, ok := byTS(items, ts)
		if !ok {
			t.Fatalf("no item %s", ts)
		}
		return it
	}

	// Alice uploads; bob receives an encrypted file and decrypts it.
	pic := testPNG(t, 64, 32)
	msg := p.files.Upload(alias, "", "a picture", []core.PendingAttachment{{Filename: "pic.png", Bytes: pic, Size: int64(len(pic))}})()
	if r, ok := msg.(ui.UploadResultMsg); !ok || r.Err != nil {
		t.Fatalf("Upload = %#v", msg)
	}
	got := bob.WaitEvent(t, room, func(e *event.Event) bool {
		return e.Type == event.EventMessage && e.Sender == aliceID && e.Content.AsMessage().MsgType == event.MsgImage
	})
	if !got.Mautrix.WasEncrypted {
		t.Error("alice's picture message was not encrypted")
	}
	c := got.Content.AsMessage()
	if c.File == nil || c.URL != "" || c.Body != "a picture" || c.FileName != "pic.png" || c.Info == nil || c.Info.Width != 64 || c.Info.Height != 32 {
		t.Fatalf("bob got %+v", c)
	}
	if !bytes.Equal(bob.DownloadFile(t, c), pic) {
		t.Fatal("bob's download is not alice's picture")
	}
	// The send mapped the event's TS (TSForSent); the echo kept it.
	sentTS, ok, err := ids.TS(ctx, room, got.ID)
	if err != nil || !ok {
		t.Fatalf("no TS for the sent event: %v %v", ok, err)
	}
	mine := itemOf(got.ID)
	if mine.TS != sentTS || mine.Text != "a picture" || len(mine.Attachments) != 1 || mine.Attachments[0].Kind != "image" {
		t.Fatalf("alice's item = %+v", mine)
	}
	path, err := p.files.Download(ctx, mine.Attachments[0].DownloadURL, mine.Attachments[0].Name)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if data, _ := os.ReadFile(path); !bytes.Equal(data, pic) {
		t.Error("alice's download is not her picture")
	}

	// Bob sends an encrypted picture; alice's pane draws it.
	bobPic := testPNG(t, 80, 40)
	theirs := itemOf(bob.SendImage(t, room, "bob.png", bobPic))
	if len(theirs.Attachments) != 1 || theirs.Text != "" {
		t.Fatalf("bob's picture item = %+v", theirs)
	}
	att := theirs.Attachments[0]
	if path, err := p.files.Download(ctx, att.DownloadURL, att.Name); err != nil {
		t.Fatalf("Download of bob's picture: %v", err)
	} else if data, _ := os.ReadFile(path); !bytes.Equal(data, bobPic) {
		t.Error("alice's download is not bob's picture")
	}

	events := make(chan tea.Msg, 8)
	pane := messages.New([]messages.MessageItem{theirs}, alias)
	pane.SetImageContext(imgrender.ImageContext{
		Protocol:   imgpkg.ProtoHalfBlock,
		Fetcher:    alice.b.ImageFetcher(),
		CellPixels: image.Pt(8, 16),
		MaxRows:    10,
		SendMsg:    func(m tea.Msg) { events <- m },
	})
	if frame := pane.View(30, 100); !strings.Contains(frame, "Loading bob.png") {
		t.Fatalf("first frame has no placeholder:\n%s", frame)
	}
	select {
	case m := <-events:
		r, ok := m.(imgrender.ImageReadyMsg)
		if !ok {
			t.Fatalf("image fetch: %#v", m)
		}
		pane.HandleImageReady(r.Channel, r.TS, r.Key)
	case <-time.After(e2e.Timeout):
		t.Fatal("the image never arrived")
	}
	frame := pane.View(30, 100)
	if strings.Contains(frame, "Loading") || strings.Count(frame, "▀") < 20 {
		t.Fatalf("second frame has no half-block picture:\n%s", frame)
	}

	// In a room that is not encrypted, a large picture has the server's
	// scaled thumbnails, through the authenticated thumbnail API.
	open := bob.CreateRoom(t, e2e.RoomOptions{Name: "open", Invite: []id.UserID{aliceID}})
	if _, err := alice.b.client.JoinRoomByID(ctx, open); err != nil {
		t.Fatalf("join: %v", err)
	}
	bob.WaitMember(t, open, aliceID, event.MembershipJoin)
	room = open
	big := itemOf(bob.SendImage(t, open, "big.png", testPNG(t, 700, 350)))
	if len(big.Attachments) != 1 || len(big.Attachments[0].Thumbs) != 3 {
		t.Fatalf("big picture item = %+v", big)
	}
	th := big.Attachments[0].Thumbs[0]
	res, err := alice.b.ImageFetcher().Fetch(ctx, imgpkg.FetchRequest{Key: big.Attachments[0].FileID + "-320", URL: th.URL})
	if err != nil {
		t.Fatalf("Fetch of the server thumbnail: %v", err)
	}
	if b := res.Img.Bounds(); th.W != 320 || th.H != 160 || b.Dx() != 320 || b.Dy() != 160 {
		t.Errorf("server thumbnail %dx%d is %v", th.W, th.H, b)
	}
}
