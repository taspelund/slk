package media

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
)

// serverThumbs are the boxes a server scales thumbnails to: Synapse's
// default thumbnail_sizes with method "scale" (the "crop" ones are
// squares, which would distort the aspect the UI sizes images by).
var serverThumbs = [][2]int{{320, 240}, {640, 480}, {800, 600}}

// Attachment is the core.Attachment of the file in c, a message of ev in
// room (ev is the event whose content c is: an edit's, when an edit
// replaced the file); false when c is not a file message or has no file.
//
// As cmd/slk's extractAttachments fills one for a Slack file: Kind
// "image" for m.image and "file" for m.file, m.video and m.audio; Name
// the file name; URL the message's matrix.to permalink; DownloadURL and
// Size; and for an image FileID (the image cache key's prefix), Mime and
// Thumbs, smallest first. Thumbs are only listed with their sizes, which
// the UI sizes the image by: an image whose message gives no size has
// none, so it shows as a link and opens by download. They are, for a
// file that is not encrypted, the server's scaled thumbnails smaller
// than the image and the image itself; for an encrypted one, the
// sender's thumbnail and the image.
func Attachment(room id.RoomID, ev id.EventID, c *event.MessageEventContent) (core.Attachment, bool) {
	switch c.MsgType {
	case event.MsgImage, event.MsgFile, event.MsgVideo, event.MsgAudio:
	default:
		return core.Attachment{}, false
	}
	src, err := SourceOf(c, PartFile)
	if err != nil {
		return core.Attachment{}, false
	}
	ref := func(p Part) Ref { return Ref{Room: room, Event: ev, Part: p} }
	att := core.Attachment{
		Kind:        "file",
		Name:        c.GetFileName(),
		URL:         room.EventURI(ev).MatrixToURL(),
		DownloadURL: ref(PartFile).URL(),
	}
	if c.Info != nil {
		att.Size = int64(c.Info.Size)
	}
	if c.MsgType != event.MsgImage {
		return att, true
	}
	att.Kind = "image"
	att.FileID = fileID(src.MXC)
	att.Mime = src.Mime
	info := c.Info
	if info == nil || info.Width <= 0 || info.Height <= 0 {
		return att, true
	}
	w, h := info.Width, info.Height
	var thumbs []core.ThumbSpec
	add := func(r Ref, tw, th int) {
		// The UI keys a cached thumb by its larger side: one per size,
		// and the image's own the largest.
		if r.Part != PartFile && max(tw, th) >= max(w, h) {
			return
		}
		for _, t := range thumbs {
			if max(t.W, t.H) == max(tw, th) {
				return
			}
		}
		thumbs = append(thumbs, core.ThumbSpec{URL: r.URL(), W: tw, H: th})
	}
	if src.File == nil {
		for _, box := range serverThumbs {
			scale := math.Min(float64(box[0])/float64(w), float64(box[1])/float64(h))
			if scale >= 1 {
				continue
			}
			r := ref(PartScaled)
			r.W, r.H = box[0], box[1]
			add(r, max(1, int(math.Round(float64(w)*scale))), max(1, int(math.Round(float64(h)*scale))))
		}
	} else if ti := info.ThumbnailInfo; ti != nil && ti.Width > 0 && ti.Height > 0 &&
		(info.ThumbnailFile != nil || info.ThumbnailURL != "") {
		add(ref(PartThumb), ti.Width, ti.Height)
	}
	add(ref(PartFile), w, h)
	att.Thumbs = thumbs
	return att, true
}

// fileID is the UI's ID of the file at mxc: "MX" and a hash, so it is a
// safe file name in the image cache and the same for every message that
// shares the file.
func fileID(mxc id.ContentURI) string {
	sum := sha256.Sum256([]byte(mxc.String()))
	return "MX" + strings.ToUpper(hex.EncodeToString(sum[:12]))
}
