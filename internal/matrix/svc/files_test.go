package svc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	imgpkg "github.com/gammons/slk/internal/image"
	"github.com/gammons/slk/internal/matrix/media"
	"github.com/gammons/slk/internal/ui"
)

// fakeMedia is an httptest homeserver's media and send API.
type fakeMedia struct {
	t     *testing.T
	mu    sync.Mutex
	files map[string][]byte // by media ID
	sent  []sentEvent
}

type sentEvent struct {
	room    id.RoomID
	id      id.EventID
	content map[string]any
	raw     json.RawMessage
}

func (f *fakeMedia) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const download = "/_matrix/client/v1/media/download/example.org/"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/_matrix/media/v3/upload":
		data, _ := io.ReadAll(r.Body)
		mediaID := fmt.Sprintf("m%d", len(f.files)+1)
		f.files[mediaID] = data
		writeJSON(f.t, w, map[string]any{"content_uri": "mxc://example.org/" + mediaID})
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/send/m.room.message/"):
		room, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/_matrix/client/v3/rooms/"), "/")
		raw, _ := io.ReadAll(r.Body)
		var content map[string]any
		_ = json.Unmarshal(raw, &content)
		ev := id.EventID(fmt.Sprintf("$sent%d", len(f.sent)+1))
		f.sent = append(f.sent, sentEvent{id.RoomID(room), ev, content, raw})
		writeJSON(f.t, w, map[string]any{"event_id": ev})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, download):
		if r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		data, ok := f.files[strings.TrimPrefix(r.URL.Path, download)]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"no"}`)
	}
}

func (f *fakeMedia) sentEvents() []sentEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentEvent(nil), f.sent...)
}

func (f *fakeMedia) uploaded() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]byte
	for i := 1; i <= len(f.files); i++ {
		out = append(out, f.files[fmt.Sprintf("m%d", i)])
	}
	return out
}

// echo is sent event i as its /sync echo.
func (f *fakeMedia) echo(t *testing.T, h *harness, i int) *event.Event {
	t.Helper()
	s := f.sentEvents()[i]
	e := lmk(t, "m.room.message", s.id, self, h.now.Add(1e12).UnixMilli(), nil, s.raw)
	e.RoomID = s.room
	return e
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, x%h, color.RGBA{255, 0, 0, 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mediaHarness is a harness whose general is encrypted, with a media
// server and the account directory set, and the backend's ports.
func mediaHarness(t *testing.T) (*harness, *fakeMedia, ports) {
	t.Helper()
	f := &fakeMedia{t: t, files: map[string][]byte{}}
	h := newHarness(t, f.handle)
	h.b.d.Dir = t.TempDir()
	resp := lsync()
	ljoin(resp, general).State.Events = []*event.Event{
		lstate(t, "m.room.encryption", "", alice, map[string]any{"algorithm": "m.megolm.v1.aes-sha2"}),
	}
	h.apply(resp)
	var p ports
	h.b.fillFiles(&p)
	if p.files == nil || p.images == nil {
		t.Fatal("fillFiles set no ports")
	}
	return h, f, p
}

func TestUploadEncryptsAndSendsEachFileWithItsTS(t *testing.T) {
	h, f, p := mediaHarness(t)
	_, rootTS := h.text(alice, "a thread")
	root, _, _ := h.ids.Event(context.Background(), general, rootTS)
	pic := testPNG(t, 40, 20)
	doc := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(doc, []byte("some notes"), 0o600); err != nil {
		t.Fatal(err)
	}

	msg := p.files.Upload(h.alias, rootTS, "see **this**", []core.PendingAttachment{
		{Filename: "pic.png", Bytes: pic, Mime: "image/png", Size: int64(len(pic))},
		{Filename: "notes.txt", Path: doc},
	})()
	if r, ok := msg.(ui.UploadResultMsg); !ok || r.Err != nil {
		t.Fatalf("Upload = %#v", msg)
	}

	up := f.uploaded()
	if len(up) != 2 || bytes.Equal(up[0], pic) || bytes.Contains(up[1], []byte("notes")) {
		t.Fatalf("the server got plaintext: %q", up)
	}
	sent := f.sentEvents()
	if len(sent) != 2 {
		t.Fatalf("sent %d events", len(sent))
	}
	img, txt := sent[0].content, sent[1].content
	if img["msgtype"] != "m.image" || img["body"] != "pic.png" || img["filename"] != "pic.png" || img["url"] != nil || img["file"] == nil {
		t.Errorf("image event = %v", img)
	}
	if info := img["info"].(map[string]any); info["w"] != 40.0 || info["h"] != 20.0 || info["mimetype"] != "image/png" || info["size"] != float64(len(pic)) {
		t.Errorf("image info = %v", info)
	}
	if txt["msgtype"] != "m.file" || txt["filename"] != "notes.txt" || txt["body"] != "see **this**" ||
		txt["formatted_body"] != "see <strong>this</strong>" || txt["file"] == nil {
		t.Errorf("file event = %v", txt)
	}
	for _, s := range sent {
		rel, _ := s.content["m.relates_to"].(map[string]any)
		if s.room != general || rel["rel_type"] != "m.thread" || rel["event_id"] != string(root) {
			t.Errorf("event %s in %s relates %v, want the thread of %s", s.id, s.room, rel, root)
		}
	}

	// Each event has its TS already; the /sync echo keeps it, and shows
	// the file, which downloads decrypted.
	ts, ok, err := h.ids.TS(context.Background(), general, sent[1].id)
	if err != nil || !ok {
		t.Fatalf("no TS for the sent event: %v %v", ok, err)
	}
	if got := h.live(f.echo(t, h, 1)); got != ts {
		t.Fatalf("echo TS = %s, want %s", got, ts)
	}
	it, ok := byTS(h.b.threadCache(h.alias, rootTS), ts)
	if !ok || len(it.Attachments) != 1 || it.Text != "see *this*" {
		t.Fatalf("echo item = %+v (%v)", it, ok)
	}
	path, err := p.files.Download(context.Background(), it.Attachments[0].DownloadURL, it.Attachments[0].Name)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "some notes" || filepath.Base(path) != "notes.txt" ||
		!strings.HasPrefix(path, filepath.Join(h.b.d.Dir, "media", "files")) {
		t.Errorf("downloaded %s: %q", path, data)
	}
}

func TestImageFetcherDrawsAnEncryptedImage(t *testing.T) {
	h, f, p := mediaHarness(t)
	pic := testPNG(t, 60, 30)
	if msg := p.files.Upload(h.alias, "", "", []core.PendingAttachment{{Filename: "p.png", Bytes: pic}})(); msg.(ui.UploadResultMsg).Err != nil {
		t.Fatalf("Upload: %v", msg)
	}
	ts := h.live(f.echo(t, h, 0))
	it, _ := byTS(h.page(), ts)
	if len(it.Attachments) != 1 || it.Attachments[0].Kind != "image" || len(it.Attachments[0].Thumbs) != 1 {
		t.Fatalf("item = %+v", it)
	}
	att := it.Attachments[0]
	if r, ok := media.ParseURL(att.Thumbs[0].URL); !ok || r.Part != media.PartFile || att.Thumbs[0].W != 60 || att.Thumbs[0].H != 30 {
		t.Errorf("thumb = %+v (an encrypted upload has only the image)", att.Thumbs[0])
	}
	res, err := p.images.Fetch(context.Background(), imgpkg.FetchRequest{Key: att.FileID + "-60", URL: att.Thumbs[0].URL})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if b := res.Img.Bounds(); b.Dx() != 60 || b.Dy() != 30 {
		t.Errorf("image is %v", b)
	}
	if h.b.ImageFetcher() != p.images {
		t.Error("ImageFetcher is not the installed fetcher")
	}
}

func TestNoMediaPortsWithoutAClientOrDirectory(t *testing.T) {
	h := newHarness(t, nil)
	var p ports
	h.b.fillFiles(&p)
	if p.files != nil || p.images != nil || h.b.ImageFetcher() != nil {
		t.Error("a store-only backend has media ports")
	}
}
