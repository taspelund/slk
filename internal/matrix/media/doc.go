// Package media fetches, decrypts, caches and uploads Matrix media for an
// account: authenticated mxc downloads (/_matrix/client/v1/media), the
// encrypted attachments of encrypted rooms, and uploads. svc/files.go
// adapts it to internal/core's FileService and ImageFetcher (WP-Media).
package media
