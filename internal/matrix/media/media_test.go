package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	imgpkg "github.com/gammons/slk/internal/image"
	"github.com/gammons/slk/internal/matrix/store"
)

const (
	room   id.RoomID = "!room:test.server"
	token            = "secret-token"
	server           = "test.server"
)

// makePNG is a w×h PNG.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type stored struct {
	data  []byte
	ctype string
}

// mediaServer is an httptest homeserver's media API: uploads, and
// authenticated downloads and thumbnails. Every request must carry the
// access token in the Authorization header.
type mediaServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	files    map[string]stored // by media ID
	thumbs   map[string]stored // server thumbnails, by media ID
	requests []string          // "METHOD path?query"
}

func newMediaServer(t *testing.T) *mediaServer {
	s := &mediaServer{t: t, files: map[string]stored{}, thumbs: map[string]stored{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *mediaServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	if r.Header.Get("Authorization") != "Bearer "+token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errcode":"M_MISSING_TOKEN","error":"no token"}`)
		return
	}
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"not found"}`)
	}
	const download, thumbnail = "/_matrix/client/v1/media/download/" + server + "/", "/_matrix/client/v1/media/thumbnail/" + server + "/"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/_matrix/media/v3/upload":
		data, _ := io.ReadAll(r.Body)
		mediaID := fmt.Sprintf("m%d", len(s.files)+1)
		s.files[mediaID] = stored{data, r.Header.Get("Content-Type")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"content_uri":"mxc://%s/%s"}`, server, mediaID)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, download):
		f, ok := s.files[strings.TrimPrefix(r.URL.Path, download)]
		if !ok {
			notFound()
			return
		}
		w.Header().Set("Content-Type", f.ctype)
		_, _ = w.Write(f.data)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, thumbnail):
		f, ok := s.thumbs[strings.TrimPrefix(r.URL.Path, thumbnail)]
		if !ok {
			notFound()
			return
		}
		w.Header().Set("Content-Type", f.ctype)
		_, _ = w.Write(f.data)
	default:
		notFound()
	}
}

func (s *mediaServer) reqs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *mediaServer) file(mxc id.ContentURIString) stored {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := mxc.Parse()
	if err != nil {
		s.t.Fatalf("parse %q: %v", mxc, err)
	}
	return s.files[u.FileID]
}

// fixture is a Client over a mediaServer, its messages in a map.
type fixture struct {
	t   *testing.T
	srv *mediaServer
	c   *Client
	dir string

	mu       sync.Mutex
	messages map[id.EventID]*event.MessageEventContent
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{t: t, srv: newMediaServer(t), dir: t.TempDir(), messages: map[id.EventID]*event.MessageEventContent{}}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "matrix.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cli, err := mautrix.NewClient(f.srv.srv.URL, "@me:"+server, token)
	if err != nil {
		t.Fatal(err)
	}
	f.c, err = New(cli, st.DB, f.dir, func(_ context.Context, r id.RoomID, ev id.EventID) (*event.MessageEventContent, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c, ok := f.messages[ev]
		if r != room || !ok {
			return nil, errors.New("no such message")
		}
		return c, nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

// post stores c as message ev.
func (f *fixture) post(ev id.EventID, c *event.MessageEventContent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// As the store has it: through JSON.
	raw, err := json.Marshal(c)
	if err != nil {
		f.t.Fatal(err)
	}
	var back event.MessageEventContent
	if err := json.Unmarshal(raw, &back); err != nil {
		f.t.Fatal(err)
	}
	f.messages[ev] = &back
}

func (f *fixture) upload(name string, data []byte, mime string, encrypt bool) *event.MessageEventContent {
	f.t.Helper()
	c, err := f.c.Upload(context.Background(), name, data, mime, encrypt)
	if err != nil {
		f.t.Fatalf("Upload: %v", err)
	}
	return c
}

func TestRefURLRoundTrips(t *testing.T) {
	for _, r := range []Ref{
		{Room: room, Event: "$a+b/c?d", Part: PartFile},
		{Room: "!x:y.z", Event: "$e", Part: PartThumb},
		{Room: room, Event: "$e", Part: PartScaled, W: 640, H: 480},
	} {
		got, ok := ParseURL(r.URL())
		if !ok || got != r {
			t.Errorf("ParseURL(%q) = %+v, %v; want %+v", r.URL(), got, ok, r)
		}
	}
	for _, s := range []string{
		"mxc://test.server/abc",
		"https://slk/file?room=!r&event=$e",
		"matrix-media://slk/other?room=!r&event=$e",
		"matrix-media://slk/file?room=!r",
		"matrix-media://slk/scaled?room=!r&event=$e",
		"matrix-media://elsewhere/file?room=!r&event=$e",
	} {
		if r, ok := ParseURL(s); ok {
			t.Errorf("ParseURL(%q) = %+v, want none", s, r)
		}
	}
}

func TestAttachmentOfEachKindOfFile(t *testing.T) {
	ev := id.EventID("$ev")
	permalink := room.EventURI(ev).MatrixToURL()
	plain := &event.MessageEventContent{
		MsgType: event.MsgImage, Body: "nice view", FileName: "view.png", URL: "mxc://test.server/img",
		Info: &event.FileInfo{MimeType: "image/png", Size: 1234, Width: 1600, Height: 900},
	}
	att, ok := Attachment(room, ev, plain)
	if !ok || att.Kind != "image" || att.Name != "view.png" || att.URL != permalink || att.Size != 1234 || att.Mime != "image/png" {
		t.Fatalf("plain image = %+v, %v", att, ok)
	}
	if !strings.HasPrefix(att.FileID, "MX") || att.FileID != fileID(id.MustParseContentURI("mxc://test.server/img")) {
		t.Errorf("FileID = %q", att.FileID)
	}
	if r, ok := ParseURL(att.DownloadURL); !ok || r != (Ref{Room: room, Event: ev, Part: PartFile}) {
		t.Errorf("DownloadURL = %q", att.DownloadURL)
	}
	var sizes []string
	for _, th := range att.Thumbs {
		r, _ := ParseURL(th.URL)
		sizes = append(sizes, fmt.Sprintf("%s %dx%d %dx%d", r.Part, r.W, r.H, th.W, th.H))
	}
	want := []string{"scaled 320x240 320x180", "scaled 640x480 640x360", "scaled 800x600 800x450", "file 0x0 1600x900"}
	if fmt.Sprint(sizes) != fmt.Sprint(want) {
		t.Errorf("plain thumbs = %q, want %q", sizes, want)
	}

	// A small image has no server thumbnail smaller than itself.
	small := *plain
	small.Info = &event.FileInfo{Width: 300, Height: 200}
	if att, _ := Attachment(room, ev, &small); len(att.Thumbs) != 1 || att.Thumbs[0].W != 300 {
		t.Errorf("small thumbs = %+v", att.Thumbs)
	}

	ef := attachment.NewEncryptedFile()
	enc := &event.MessageEventContent{
		MsgType: event.MsgImage, Body: "secret.png",
		File: &event.EncryptedFileInfo{EncryptedFile: *ef, URL: "mxc://test.server/enc"},
		Info: &event.FileInfo{MimeType: "image/png", Width: 1000, Height: 2000,
			ThumbnailInfo: &event.FileInfo{Width: 400, Height: 800},
			ThumbnailFile: &event.EncryptedFileInfo{EncryptedFile: *attachment.NewEncryptedFile(), URL: "mxc://test.server/encthumb"}},
	}
	att, ok = Attachment(room, ev, enc)
	if !ok || att.Kind != "image" || att.Name != "secret.png" || len(att.Thumbs) != 2 {
		t.Fatalf("encrypted image = %+v, %v", att, ok)
	}
	if r, _ := ParseURL(att.Thumbs[0].URL); r.Part != PartThumb || att.Thumbs[0].W != 400 || att.Thumbs[0].H != 800 {
		t.Errorf("encrypted thumb = %+v", att.Thumbs[0])
	}
	if r, _ := ParseURL(att.Thumbs[1].URL); r.Part != PartFile || att.Thumbs[1].W != 1000 {
		t.Errorf("encrypted file thumb = %+v", att.Thumbs[1])
	}
	for _, s := range []string{att.URL, att.DownloadURL, att.Thumbs[0].URL, att.Thumbs[1].URL} {
		if strings.Contains(s, ef.Key.Key) || strings.Contains(s, "mxc") {
			t.Errorf("URL %q gives the file away", s)
		}
	}

	// No size: no thumbs, so the UI shows a link and opens by download.
	sizeless := &event.MessageEventContent{MsgType: event.MsgImage, Body: "x.gif", URL: "mxc://test.server/x"}
	if att, ok := Attachment(room, ev, sizeless); !ok || att.Kind != "image" || att.FileID == "" || att.Thumbs != nil {
		t.Errorf("sizeless image = %+v, %v", att, ok)
	}

	for _, mt := range []event.MessageType{event.MsgFile, event.MsgVideo, event.MsgAudio} {
		c := &event.MessageEventContent{MsgType: mt, Body: "clip", URL: "mxc://test.server/f", Info: &event.FileInfo{MimeType: "video/mp4", Size: 9, Width: 10, Height: 10}}
		att, ok := Attachment(room, ev, c)
		if !ok || att.Kind != "file" || att.Name != "clip" || att.Size != 9 || att.FileID != "" || att.Thumbs != nil || att.DownloadURL == "" || att.URL != permalink {
			t.Errorf("%s = %+v, %v", mt, att, ok)
		}
	}
	for _, c := range []*event.MessageEventContent{
		{MsgType: event.MsgText, Body: "hi"},
		{MsgType: event.MsgFile, Body: "no file"},
		{MsgType: event.MsgImage, Body: "bad", URL: "not-an-mxc"},
	} {
		if att, ok := Attachment(room, ev, c); ok {
			t.Errorf("Attachment(%+v) = %+v", c, att)
		}
	}
}

func TestEncryptedAttachmentRoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	pic := makePNG(t, 64, 48)

	c := f.upload("cat.png", pic, "", true)
	if c.URL != "" || c.File == nil || c.File.URL == "" || c.MsgType != event.MsgImage || c.FileName != "cat.png" {
		t.Fatalf("content = %+v", c)
	}
	if c.Info.MimeType != "image/png" || c.Info.Size != len(pic) || c.Info.Width != 64 || c.Info.Height != 48 {
		t.Errorf("info = %+v", c.Info)
	}
	up := f.srv.file(c.File.URL)
	if bytes.Equal(up.data, pic) || len(up.data) != len(pic) || up.ctype != "application/octet-stream" {
		t.Fatalf("the server got %d bytes of %q, the plaintext: %v", len(up.data), up.ctype, bytes.Equal(up.data, pic))
	}
	if reqs := f.srv.reqs(); len(reqs) != 1 || strings.Contains(reqs[0], "cat") {
		t.Errorf("upload requests = %q (the name must not reach the server)", reqs)
	}

	f.post("$cat", c)
	att, ok := Attachment(room, "$cat", f.messages["$cat"])
	if !ok {
		t.Fatal("no attachment")
	}

	// Read and Download decrypt.
	ref, _ := ParseURL(att.DownloadURL)
	got, mime, err := f.c.Read(ctx, ref)
	if err != nil || !bytes.Equal(got, pic) || mime != "image/png" {
		t.Fatalf("Read = %d bytes, %q, %v", len(got), mime, err)
	}
	path, err := f.c.Download(ctx, att.DownloadURL, att.Name)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if want := filepath.Join(f.dir, "media", "files"); filepath.Dir(filepath.Dir(path)) != want || filepath.Base(path) != "cat.png" {
		t.Errorf("path = %s, want %s/<hash>/cat.png", path, want)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, pic) {
		t.Errorf("saved file: %d bytes, %v", len(data), err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("saved file mode: %v, %v", st.Mode(), err)
	}

	// A second Download is the cached file.
	n := len(f.srv.reqs())
	if again, err := f.c.Download(ctx, att.DownloadURL, att.Name); err != nil || again != path {
		t.Errorf("second Download = %q, %v", again, err)
	}
	if len(f.srv.reqs()) != n {
		t.Errorf("second Download asked the server: %q", f.srv.reqs()[n:])
	}

	// The image fetcher draws it, through the authenticated download.
	th := att.Thumbs[len(att.Thumbs)-1]
	res, err := f.c.Images().Fetch(ctx, imgpkg.FetchRequest{Key: att.FileID + "-64", URL: th.URL, Target: image.Pt(32, 24)})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if b := res.Img.Bounds(); b.Dx() != 32 || b.Dy() != 24 {
		t.Errorf("fetched image is %v", b)
	}
	if img, ok := f.c.Images().Cached(att.FileID+"-64", image.Pt(32, 24)); !ok || img == nil {
		t.Error("the fetched image is not cached")
	}
	if !strings.HasPrefix(res.Source, filepath.Join(f.dir, "media", "images")+string(filepath.Separator)) {
		t.Errorf("image cached at %s", res.Source)
	}

	// Ciphertext that does not match its hash is refused.
	f.srv.mu.Lock()
	u, _ := c.File.URL.Parse()
	bad := f.srv.files[u.FileID]
	bad.data = append([]byte{bad.data[0] ^ 1}, bad.data[1:]...)
	f.srv.files[u.FileID] = bad
	f.srv.mu.Unlock()
	if _, _, err := f.c.Read(ctx, ref); !errors.Is(err, attachment.ErrHashMismatch) {
		t.Errorf("Read of tampered file: %v, want a hash mismatch", err)
	}
}

func TestPlainUploadAndServerThumbnails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	pic := makePNG(t, 1000, 500)
	c := f.upload("wide.png", pic, "image/png", false)
	if c.File != nil || c.URL == "" || c.Info.Width != 1000 || c.Info.Height != 500 {
		t.Fatalf("content = %+v", c)
	}
	if up := f.srv.file(c.URL); !bytes.Equal(up.data, pic) || up.ctype != "image/png" {
		t.Errorf("server got %q", up.ctype)
	}
	if reqs := f.srv.reqs(); len(reqs) != 1 || !strings.Contains(reqs[0], "filename=wide.png") {
		t.Errorf("upload requests = %q", reqs)
	}
	f.post("$wide", c)
	att, _ := Attachment(room, "$wide", f.messages["$wide"])
	if len(att.Thumbs) != 4 {
		t.Fatalf("thumbs = %+v", att.Thumbs)
	}
	scaled, _ := ParseURL(att.Thumbs[0].URL)

	// No server thumbnail: the file itself.
	got, mime, err := f.c.Read(ctx, scaled)
	if err != nil || !bytes.Equal(got, pic) || mime != "image/png" {
		t.Fatalf("Read without a thumbnail = %d bytes, %q, %v", len(got), mime, err)
	}
	// With one, the thumbnail.
	thumb := makePNG(t, 320, 160)
	u, _ := c.URL.Parse()
	f.srv.mu.Lock()
	f.srv.thumbs[u.FileID] = stored{thumb, "image/png"}
	f.srv.mu.Unlock()
	got, _, err = f.c.Read(ctx, scaled)
	if err != nil || !bytes.Equal(got, thumb) {
		t.Fatalf("Read of the thumbnail = %d bytes, %v", len(got), err)
	}
	last := f.srv.reqs()[len(f.srv.reqs())-1]
	if want := "GET /_matrix/client/v1/media/thumbnail/" + server + "/" + u.FileID + "?"; !strings.HasPrefix(last, want) ||
		!strings.Contains(last, "width=320") || !strings.Contains(last, "height=240") || !strings.Contains(last, "method=scale") {
		t.Errorf("thumbnail request = %q", last)
	}
	for _, r := range f.srv.reqs() {
		if strings.Contains(r, "/v3/download") || strings.Contains(r, "/v3/thumbnail") {
			t.Errorf("unauthenticated media endpoint used: %q", r)
		}
	}
}

func TestReadFailsForUnknownMessagesAndOtherFiles(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, _, err := f.c.Read(ctx, Ref{Room: room, Event: "$nope", Part: PartFile}); err == nil {
		t.Error("Read of a message not stored worked")
	}
	f.post("$text", &event.MessageEventContent{MsgType: event.MsgText, Body: "hi"})
	if _, _, err := f.c.Read(ctx, Ref{Room: room, Event: "$text", Part: PartFile}); !errors.Is(err, ErrNoFile) {
		t.Errorf("Read of a text message: %v", err)
	}
	enc := f.upload("x.png", makePNG(t, 2, 2), "", true)
	f.post("$enc", enc)
	if _, _, err := f.c.Read(ctx, Ref{Room: room, Event: "$enc", Part: PartScaled, W: 1, H: 1}); !errors.Is(err, ErrNoFile) {
		t.Errorf("server thumbnail of an encrypted file: %v", err)
	}
	if _, err := f.c.Download(ctx, "https://example.com/x", "x"); err == nil {
		t.Error("Download of a URL not ours worked")
	}
	if _, err := f.c.Images().Fetch(ctx, imgpkg.FetchRequest{Key: "k", URL: "https://example.com/x.png"}); err == nil {
		t.Error("the image fetcher fetched a URL not ours")
	}
}

func TestDownloadEvictsLeastRecentlyUsed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.c.fileCap = 25
	clock := time.Unix(1_000_000, 0)
	f.c.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	var paths []string
	for i, ev := range []id.EventID{"$a", "$b", "$c"} {
		f.post(ev, f.upload(fmt.Sprintf("f%d.txt", i), bytes.Repeat([]byte{'x'}, 10), "text/plain", i == 1))
		att, _ := Attachment(room, ev, f.messages[ev])
		if i == 2 {
			// Use a again, so b is the least recently used.
			if _, err := f.c.Download(ctx, Ref{Room: room, Event: "$a", Part: PartFile}.URL(), "f0.txt"); err != nil {
				t.Fatal(err)
			}
		}
		p, err := f.c.Download(ctx, att.DownloadURL, att.Name)
		if err != nil {
			t.Fatalf("Download %s: %v", ev, err)
		}
		paths = append(paths, p)
	}
	for i, want := range []bool{true, false, true} {
		if _, err := os.Stat(paths[i]); (err == nil) != want {
			t.Errorf("file %d on disk: %v, want %v", i, err == nil, want)
		}
	}
	var n int
	var total int64
	if err := f.c.db.QueryRow(ctx, `SELECT COUNT(*), SUM(size) FROM slkmx_media_cache_index`).Scan(&n, &total); err != nil || n != 2 || total != 20 {
		t.Errorf("index has %d rows, %d bytes (%v)", n, total, err)
	}
	if _, err := os.Stat(filepath.Dir(paths[1])); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("evicted file's directory: %v", err)
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf": "report.pdf", "../../etc/passwd": "_.._etc_passwd", " .hidden ": "hidden",
		"a\x00b": "a_b", "": "download", "...": "download",
	} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}
