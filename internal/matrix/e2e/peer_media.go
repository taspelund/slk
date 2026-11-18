//go:build matrixe2e

package e2e

import (
	"bytes"
	"image"
	_ "image/png" // DecodeConfig of the pictures SendImage sends
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// SendImage uploads data, a PNG named name, and sends it to room as an
// m.image the way Element does: in an encrypted room the file is
// encrypted (an EncryptedFile in "file", the upload application/
// octet-stream), otherwise it is uploaded as is ("url"). It returns the
// event ID.
func (p *Peer) SendImage(t testing.TB, room id.RoomID, name string, data []byte) id.EventID {
	t.Helper()
	ctx := Ctx(t)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("e2e: %s: %s is not a PNG: %v", p.UserID, name, err)
	}
	content := &event.MessageEventContent{
		MsgType: event.MsgImage, Body: name, FileName: name,
		Info: &event.FileInfo{MimeType: "image/png", Size: len(data), Width: cfg.Width, Height: cfg.Height},
	}
	encrypted, err := p.Client.StateStore.IsEncrypted(ctx, room)
	if err != nil {
		t.Fatalf("e2e: %s: is %s encrypted: %v", p.UserID, room, err)
	}
	req := mautrix.ReqUploadMedia{ContentBytes: data, ContentType: "image/png", FileName: name}
	var ef *attachment.EncryptedFile
	if encrypted {
		ef = attachment.NewEncryptedFile()
		req = mautrix.ReqUploadMedia{ContentBytes: ef.Encrypt(data), ContentType: "application/octet-stream"}
	}
	resp, err := p.Client.UploadMedia(ctx, req)
	if err != nil {
		t.Fatalf("e2e: %s: upload %s: %v", p.UserID, name, err)
	}
	if ef != nil {
		content.File = &event.EncryptedFileInfo{EncryptedFile: *ef, URL: resp.ContentURI.CUString()}
	} else {
		content.URL = resp.ContentURI.CUString()
	}
	return p.Send(t, room, content)
}

// DownloadFile downloads the file of a media message's content through
// the authenticated media API and returns its plaintext, decrypting an
// encrypted one.
func (p *Peer) DownloadFile(t testing.TB, c *event.MessageEventContent) []byte {
	t.Helper()
	raw := c.URL
	if c.File != nil {
		raw = c.File.URL
	}
	mxc, err := raw.Parse()
	if err != nil {
		t.Fatalf("e2e: %s: file URL %q: %v", p.UserID, raw, err)
	}
	data, err := p.Client.DownloadBytes(Ctx(t), mxc)
	if err != nil {
		t.Fatalf("e2e: %s: download %s: %v", p.UserID, mxc, err)
	}
	if c.File != nil {
		if data, err = c.File.Decrypt(data); err != nil {
			t.Fatalf("e2e: %s: decrypt %s: %v", p.UserID, mxc, err)
		}
	}
	return data
}
