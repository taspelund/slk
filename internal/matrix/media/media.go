package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	imgpkg "github.com/gammons/slk/internal/image"
)

// Part is which file of a message a Ref reads.
type Part string

const (
	// PartFile is the message's file.
	PartFile Part = "file"
	// PartThumb is the thumbnail its sender attached (info.thumbnail_file
	// or info.thumbnail_url).
	PartThumb Part = "thumb"
	// PartScaled is the server's thumbnail of the file, scaled to fit W×H.
	// Only for files that are not encrypted: the server cannot read the
	// others. A server that cannot make one serves the file instead.
	PartScaled Part = "scaled"
)

// urlScheme and urlHost make a Ref's URL. Not a scheme anything else
// handles: only this package reads it.
const (
	urlScheme = "matrix-media"
	urlHost   = "slk"
)

// Ref names one file of a stored message.
type Ref struct {
	Room  id.RoomID
	Event id.EventID
	Part  Part
	// W and H are the box a PartScaled thumbnail fits in.
	W, H int
}

// URL is r as a URL for the UI.
func (r Ref) URL() string {
	q := url.Values{}
	q.Set("room", string(r.Room))
	q.Set("event", string(r.Event))
	if r.Part == PartScaled {
		q.Set("w", strconv.Itoa(r.W))
		q.Set("h", strconv.Itoa(r.H))
	}
	return (&url.URL{Scheme: urlScheme, Host: urlHost, Path: "/" + string(r.Part), RawQuery: q.Encode()}).String()
}

// ParseURL is the Ref of a URL made by Ref.URL; false for any other.
func ParseURL(s string) (Ref, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != urlScheme || u.Host != urlHost {
		return Ref{}, false
	}
	q := u.Query()
	r := Ref{Room: id.RoomID(q.Get("room")), Event: id.EventID(q.Get("event")), Part: Part(strings.TrimPrefix(u.Path, "/"))}
	if r.Room == "" || r.Event == "" {
		return Ref{}, false
	}
	switch r.Part {
	case PartFile, PartThumb:
	case PartScaled:
		r.W, _ = strconv.Atoi(q.Get("w"))
		r.H, _ = strconv.Atoi(q.Get("h"))
		if r.W <= 0 || r.H <= 0 {
			return Ref{}, false
		}
	default:
		return Ref{}, false
	}
	return r, true
}

// Source is where a file is and how to read it.
type Source struct {
	MXC id.ContentURI
	// File decrypts it; nil when it is not encrypted.
	File *attachment.EncryptedFile
	// Mime is the type its message gives, "" if none.
	Mime string
}

// ErrNoFile says a message has no such file.
var ErrNoFile = errors.New("media: the message has no such file")

// SourceOf is part's source in the message content c.
func SourceOf(c *event.MessageEventContent, part Part) (Source, error) {
	var mime string
	if c.Info != nil {
		mime = c.Info.MimeType
	}
	switch part {
	case PartFile:
		return source(c.URL, c.File, mime)
	case PartScaled:
		if c.File != nil {
			return Source{}, ErrNoFile
		}
		return source(c.URL, nil, mime)
	case PartThumb:
		if c.Info == nil {
			return Source{}, ErrNoFile
		}
		if c.Info.ThumbnailInfo != nil {
			mime = c.Info.ThumbnailInfo.MimeType
		} else {
			mime = ""
		}
		return source(c.Info.ThumbnailURL, c.Info.ThumbnailFile, mime)
	}
	return Source{}, ErrNoFile
}

func source(plain id.ContentURIString, enc *event.EncryptedFileInfo, mime string) (Source, error) {
	raw, s := plain, Source{Mime: mime}
	if enc != nil {
		f := enc.EncryptedFile
		raw, s.File = enc.URL, &f
	}
	if raw == "" {
		return Source{}, ErrNoFile
	}
	mxc, err := raw.Parse()
	if err != nil {
		return Source{}, fmt.Errorf("media: %w", err)
	}
	s.MXC = mxc
	return s, nil
}

// Lookup returns the content of a stored message: the event's own, or
// its m.new_content when the event is an edit. It fails for an event
// that is not stored or was redacted.
type Lookup func(ctx context.Context, room id.RoomID, ev id.EventID) (*event.MessageEventContent, error)

const (
	// ImageCacheMB caps media/images.
	ImageCacheMB = 256
	// FileCacheBytes caps media/files.
	FileCacheBytes = 1 << 30
	// fetchTimeout bounds one image fetch.
	fetchTimeout = 2 * time.Minute
)

// Client is an account's media.
type Client struct {
	cli    *mautrix.Client
	db     *dbutil.Database
	lookup Lookup
	dir    string // media/files
	// fileCap is the cap of media/files, FileCacheBytes but in tests.
	fileCap int64
	// now is the clock of last_used; time.Now but in tests.
	now    func() time.Time
	images *imgpkg.Fetcher
}

// New returns the media of the account cli is logged in to, caching
// under dir/media. db is the account database (slkmx_media_cache_index);
// lookup reads stored messages.
func New(cli *mautrix.Client, db *dbutil.Database, dir string, lookup Lookup) (*Client, error) {
	root := filepath.Join(dir, "media")
	files := filepath.Join(root, "files")
	for _, d := range []string{root, files} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("media: %w", err)
		}
	}
	cache, err := imgpkg.NewCache(filepath.Join(root, "images"), ImageCacheMB)
	if err != nil {
		return nil, fmt.Errorf("media: image cache: %w", err)
	}
	c := &Client{cli: cli, db: db, lookup: lookup, dir: files, fileCap: FileCacheBytes, now: time.Now}
	c.images = imgpkg.NewFetcher(cache, &http.Client{Transport: imageTransport{c}, Timeout: fetchTimeout})
	return c, nil
}

// Images is the account's core.ImageFetcher: internal/image's fetcher,
// reading Ref URLs through Read.
func (c *Client) Images() *imgpkg.Fetcher { return c.images }

// Read returns the plaintext of the file r names and its MIME type ("" if
// neither the message nor the server says).
func (c *Client) Read(ctx context.Context, r Ref) ([]byte, string, error) {
	src, err := c.source(ctx, r)
	if err != nil {
		return nil, "", err
	}
	if r.Part == PartScaled {
		data, mime, err := c.thumbnail(ctx, src.MXC, r.W, r.H)
		if err == nil {
			return data, mime, nil
		}
		// Not every file has a server thumbnail (an SVG, a server that
		// does not make them): show the file itself.
	}
	return c.read(ctx, src)
}

// source is r's source, from its stored message.
func (c *Client) source(ctx context.Context, r Ref) (Source, error) {
	content, err := c.lookup(ctx, r.Room, r.Event)
	if err != nil {
		return Source{}, err
	}
	return SourceOf(content, r.Part)
}

// read downloads src and decrypts it.
func (c *Client) read(ctx context.Context, src Source) ([]byte, string, error) {
	resp, err := c.cli.Download(ctx, src.MXC)
	if err != nil {
		return nil, "", fmt.Errorf("media: download %s: %w", src.MXC, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("media: download %s: %w", src.MXC, err)
	}
	mime := src.Mime
	if src.File != nil {
		if data, err = src.File.Decrypt(data); err != nil {
			return nil, "", fmt.Errorf("media: decrypt %s: %w", src.MXC, err)
		}
	} else if mime == "" {
		mime = resp.Header.Get("Content-Type")
	}
	return data, mime, nil
}

// thumbnail downloads the server's thumbnail of mxc, scaled to fit w×h.
func (c *Client) thumbnail(ctx context.Context, mxc id.ContentURI, w, h int) ([]byte, string, error) {
	resp, err := c.cli.DownloadThumbnail(ctx, mxc, h, w, mautrix.DownloadThumbnailExtra{Method: "scale"})
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return data, resp.Header.Get("Content-Type"), nil
}
