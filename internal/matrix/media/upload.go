package media

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif" // image.DecodeConfig of uploaded images
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	_ "golang.org/x/image/webp"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
)

// Upload uploads data, the file name, and returns the message content
// that shares it, with no caption: body and filename are name. When
// encrypt is set (an encrypted room), the file is encrypted first and
// the content carries it as file (key, iv, hash), as the spec's
// encrypted attachments are; the server only sees ciphertext.
//
// mimeType may be empty: the name's extension or the data's first bytes
// decide it. The msgtype follows it (m.image, m.video, m.audio, else
// m.file); info has mimetype and size, and for an image whose size
// decodes, w and h. No thumbnail is made.
func (c *Client) Upload(ctx context.Context, name string, data []byte, mimeType string, encrypt bool) (*event.MessageEventContent, error) {
	if mimeType == "" {
		mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	content := &event.MessageEventContent{
		MsgType:  msgTypeOf(mimeType),
		Body:     name,
		FileName: name,
		Info:     &event.FileInfo{MimeType: mimeType, Size: len(data)},
		Mentions: &event.Mentions{},
	}
	if content.MsgType == event.MsgImage {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			content.Info.Width, content.Info.Height = cfg.Width, cfg.Height
		}
	}
	req := mautrix.ReqUploadMedia{ContentBytes: data, ContentType: mimeType, FileName: name}
	var ef *attachment.EncryptedFile
	if encrypt {
		ef = attachment.NewEncryptedFile()
		cipher := bytes.Clone(data)
		ef.EncryptInPlace(cipher)
		// The name and type would tell the server what the file is.
		req = mautrix.ReqUploadMedia{ContentBytes: cipher, ContentType: "application/octet-stream"}
	}
	resp, err := c.cli.UploadMedia(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("media: upload %s: %w", name, err)
	}
	if ef != nil {
		content.File = &event.EncryptedFileInfo{EncryptedFile: *ef, URL: resp.ContentURI.CUString()}
	} else {
		content.URL = resp.ContentURI.CUString()
	}
	return content, nil
}

// msgTypeOf is the msgtype of a file of MIME type t.
func msgTypeOf(t string) event.MessageType {
	switch {
	case strings.HasPrefix(t, "image/"):
		return event.MsgImage
	case strings.HasPrefix(t, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(t, "audio/"):
		return event.MsgAudio
	}
	return event.MsgFile
}
