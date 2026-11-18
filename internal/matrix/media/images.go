package media

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// imageTransport is the http.RoundTripper of the image fetcher's client:
// internal/image's Fetcher (singleflight, bounded concurrency, disk
// cache, decode, prerender) asks for a Ref URL, and this answers with
// the file's plaintext, as if a server had sent it.
type imageTransport struct{ c *Client }

func (t imageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ref, ok := ParseURL(req.URL.String())
	if !ok {
		return nil, fmt.Errorf("media: not a media URL: %s", req.URL.Redacted())
	}
	data, mime, err := t.c.Read(req.Context(), ref)
	if err != nil {
		return nil, err
	}
	// The fetcher takes only image/* answers; a message without a
	// mimetype (or a server saying application/octet-stream) still
	// carries an image when it sniffs as one.
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		mime = http.DetectContentType(data)
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {mime}},
		Body:          io.NopCloser(bytes.NewReader(data)),
		ContentLength: int64(len(data)),
		Request:       req,
	}, nil
}
