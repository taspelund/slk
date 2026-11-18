package svc

// WP-Media: uploads, downloads, images. This file is the work package's; it owns filesState
// (the Backend's files field) and fillFiles.
//
//	FileService.Upload    one message per attachment (Matrix has no
//	                      multi-file message), the caption on the last as
//	                      its body; files encrypted in an encrypted room.
//	                      Each sent event gets its TS from TSForSent, so its
//	                      /sync echo is the same row. UploadResultMsg; no
//	                      UploadProgressMsg (the backend has no way to send
//	                      one outside Run).
//	FileService.Download  media.Client.Download: saves the file under the
//	                      account directory (media/files) and returns it.
//	ImageFetcher          media.Client.Images: inline images and previews
//	                      of Attachment thumbnails, through internal/image.
//
// Attachments themselves are items.go's, from media.Attachment.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/debuglog"
	"github.com/gammons/slk/internal/matrix/media"
	"github.com/gammons/slk/internal/matrix/mxfmt"
	"github.com/gammons/slk/internal/ui"
)

// uploadTimeout bounds one Upload batch.
const uploadTimeout = 5 * time.Minute

// filesState is WP-Media's state on a Backend.
type filesState struct {
	once  sync.Once
	media *media.Client // nil when the backend has no client or no directory
}

func newFilesState() *filesState { return &filesState{} }

// fillFiles sets WP-Media's ports on p.
func (b *Backend) fillFiles(p *ports) {
	m := b.media()
	if m == nil {
		return
	}
	p.files = core.NewFileService(b.upload, m.Download)
	p.images = m.Images()
}

// ImageFetcher is the backend's core.ImageFetcher, which Install sets on
// the App, for the App's image context (imgrender.ImageContext.Fetcher);
// nil when the backend has none.
func (b *Backend) ImageFetcher() core.ImageFetcher {
	if m := b.media(); m != nil {
		return m.Images()
	}
	return nil
}

// media is the account's media client, made on first use.
func (b *Backend) media() *media.Client {
	f := b.files
	f.once.Do(func() {
		if b.client == nil || b.d.Dir == "" || b.d.Store == nil {
			return
		}
		m, err := media.New(b.client, b.d.Store.DB, b.d.Dir, b.mediaContent)
		if err != nil {
			debuglog.General("matrix media: %v", err)
			return
		}
		f.media = m
	})
	return f.media
}

// errNoMessage says a media URL names no stored, unredacted message.
var errNoMessage = errors.New("matrix media: no such message")

// mediaContent is media.Lookup over the store.
func (b *Backend) mediaContent(ctx context.Context, room id.RoomID, ev id.EventID) (*event.MessageEventContent, error) {
	e, ok, err := b.d.Store.GetEvent(ctx, room, ev)
	if err != nil {
		return nil, err
	}
	if !ok || e.Redacted || e.Type != event.EventMessage.Type {
		return nil, errNoMessage
	}
	c := parseMessage(e.Content)
	if c.NewContent != nil {
		return c.NewContent, nil
	}
	return c, nil
}

// upload is FileService.Upload.
func (b *Backend) upload(channelID, threadTS, caption string, atts []core.PendingAttachment) core.Cmd {
	return func() core.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
		defer cancel()
		return ui.UploadResultMsg{Err: b.sendFiles(ctx, channelID, threadTS, caption, atts)}
	}
}

// sendFiles sends each attachment as a message of the room channelID
// names, in a thread when threadTS is set, with caption on the last.
func (b *Backend) sendFiles(ctx context.Context, channelID, threadTS, caption string, atts []core.PendingAttachment) error {
	m := b.media()
	if m == nil {
		return errors.New("matrix media: unavailable")
	}
	room, ok := b.rooms.Room(channelID)
	if !ok {
		return fmt.Errorf("matrix media: unknown conversation %s", channelID)
	}
	encrypt := room.Encrypted
	if !encrypt && b.client.StateStore != nil {
		encrypt, _ = b.client.StateStore.IsEncrypted(ctx, room.ID)
	}
	var root id.EventID
	if threadTS != "" {
		ev, ok, err := b.d.IDs.Event(ctx, room.ID, threadTS)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("matrix media: unknown thread %s", threadTS)
		}
		root = ev
	}
	for i, att := range atts {
		data := att.Bytes
		if data == nil {
			var err error
			if data, err = os.ReadFile(att.Path); err != nil {
				return fmt.Errorf("opening %s: %w", att.Filename, err)
			}
		}
		content, err := m.Upload(ctx, att.Filename, data, att.Mime, encrypt)
		if err != nil {
			return err
		}
		if i == len(atts)-1 && caption != "" {
			c := mxfmt.FromCompose(caption, b.rooms.Resolver())
			content.Body, content.Format, content.FormattedBody, content.Mentions = c.Body, c.Format, c.FormattedBody, c.Mentions
		}
		if root != "" {
			content.RelatesTo = (&event.RelatesTo{}).SetThread(root, root)
		}
		resp, err := b.client.SendMessageEvent(ctx, room.ID, event.EventMessage, content)
		if err != nil {
			return fmt.Errorf("sending %s: %w", att.Filename, err)
		}
		if _, err := b.d.IDs.TSForSent(ctx, room.ID, resp.EventID, time.Now()); err != nil {
			return err
		}
	}
	return nil
}
